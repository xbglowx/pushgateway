package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/bmizerany/pat"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/pushgateway/handler"
	"github.com/prometheus/pushgateway/storage"
)

var (
	addr                = flag.String("addr", ":8080", "Address to listen on.")
	persistenceFile     = flag.String("persistence.file", "", "File to persist metrics. If empty, metrics are only kept in memory.")
	persistenceDuration = flag.Duration("persistence.duration", 5*time.Minute, "Do not write the persistence file more often than that.")

	memStats        runtime.MemStats
	internalMetrics = []*struct {
		name   string
		help   string
		eval   func() float64
		metric prometheus.Metric
	}{
		{
			name:   "instance_goroutine_count",
			help:   "The number of goroutines that currently exist.",
			eval:   func() float64 { return float64(runtime.NumGoroutine()) },
			metric: prometheus.NewGauge(),
			// Not a counter, despite the name... It can go up and down.
		},
		{
			name:   "instance_allocated_bytes",
			help:   "Bytes allocated and still in use.",
			eval:   func() float64 { return float64(memStats.Alloc) },
			metric: prometheus.NewGauge(),
		},
		{
			name:   "instance_total_allocated_bytes",
			help:   "Bytes allocated (even if freed).",
			eval:   func() float64 { return float64(memStats.TotalAlloc) },
			metric: prometheus.NewGauge(),
		},
		{
			name:   "instance_heap_allocated_bytes",
			help:   "Heap bytes allocated and still in use.",
			eval:   func() float64 { return float64(memStats.HeapAlloc) },
			metric: prometheus.NewGauge(),
		},
		{
			name:   "instance_gc_high_watermark_bytes",
			help:   "Next run in HeapAlloc time (bytes).",
			eval:   func() float64 { return float64(memStats.NextGC) },
			metric: prometheus.NewGauge(),
		},
		{
			name:   "instance_gc_total_pause_ns",
			help:   "Total GC paise time.",
			eval:   func() float64 { return float64(memStats.PauseTotalNs) },
			metric: prometheus.NewGauge(),
		},
		{
			name:   "instance_gc_count",
			help:   "GC count.",
			eval:   func() float64 { return float64(memStats.NumGC) },
			metric: prometheus.NewCounter(),
		},
	}
)

func main() {
	flag.Parse()
	mux := pat.New()

	ms := storage.NewDiskMetricStore(*persistenceFile, *persistenceDuration)

	prometheus.DefaultRegistry.SetMetricFamilyInjectionHook(ms.GetMetricFamilies)

	// The following demonstrate clearly the clunkiness of the current Go
	// client library when it comes to values that are owned by other parts
	// of the program and have to be evaluated on the fly.
	registerInternalMetrics()
	mux.Get("/metrics", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			updateInternalMetrics()
			prometheus.DefaultHandler(w, r)
		}))

	mux.Put("/metrics/job/:job/instance/:instance", handler.Push(ms))
	mux.Post("/metrics/job/:job/instance/:instance", handler.Push(ms))
	mux.Del("/metrics/job/:job/instance/:instance", handler.Delete(ms))
	mux.Put("/metrics/job/:job", handler.Push(ms))
	mux.Post("/metrics/job/:job", handler.Push(ms))
	mux.Del("/metrics/job/:job", handler.Delete(ms))
	// TODO: Add web interface

	http.Handle("/", mux)

	log.Printf("Listening on %s.\n", *addr)
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	go interruptHandler(l)
	err = (&http.Server{Addr: *addr}).Serve(l)
	log.Print("HTTP server stopped: ", err)
	// To give running connections a chance to submit their payload, we wait
	// for 1sec, but we don't want to wait long (e.g. until all connections
	// are done) to not delay the shutdown.
	time.Sleep(time.Second)
	if err := ms.Shutdown(); err != nil {
		log.Print("Problem shutting down metric storage: ", err)
	}
}

func interruptHandler(l net.Listener) {
	notifier := make(chan os.Signal)
	signal.Notify(notifier, os.Interrupt, syscall.SIGTERM)
	<-notifier
	log.Print("Received SIGINT/SIGTERM; exiting gracefully...")
	l.Close()
}

func registerInternalMetrics() {
	for _, im := range internalMetrics {
		prometheus.Register(im.name, im.help, nil, im.metric)
	}
}

func updateInternalMetrics() {
	runtime.ReadMemStats(&memStats)
	for _, im := range internalMetrics {
		switch m := im.metric.(type) {
		case prometheus.Gauge:
			m.Set(nil, im.eval())
		case prometheus.Counter:
			m.Set(nil, im.eval())
		default:
			log.Print("Unexpected metric type: ", m)
		}
	}
}
