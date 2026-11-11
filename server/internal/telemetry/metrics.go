// Package telemetry owns the process-local Prometheus registry.
package telemetry

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var latencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .2, .3, .5, 1, 2, 5}

type Metrics struct {
	registry     *prometheus.Registry
	httpDuration *prometheus.HistogramVec

	WSConnections    prometheus.Gauge
	WSFrames         *prometheus.CounterVec
	WSQueueOverflows prometheus.Counter
	WSResyncs        prometheus.Counter
	WSLiveDelivery   prometheus.Histogram

	OutboxBatchDuration *prometheus.HistogramVec
	OutboxPublished     prometheus.Counter
	OutboxFailures      prometheus.Counter
	OutboxOldestPending prometheus.Gauge
	OutboxProbeOK       prometheus.Gauge
}

func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "jacko_http_request_duration_seconds",
			Help:    "Duration of message API requests.",
			Buckets: latencyBuckets,
		}, []string{"route", "code"}),
		WSConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jacko_ws_connections",
			Help: "Authenticated WebSocket connections serving live events.",
		}),
		WSFrames: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jacko_ws_dispatch_total",
			Help: "Event frames written to WebSocket connections by delivery mode.",
		}, []string{"mode"}),
		WSQueueOverflows: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jacko_ws_queue_overflows_total",
			Help: "Local subscriptions dropped because their live queue was full.",
		}),
		WSResyncs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jacko_ws_resync_required_total",
			Help: "Connections requiring a fresh history snapshot.",
		}),
		WSLiveDelivery: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "jacko_ws_live_delivery_seconds",
			Help:    "Time from gateway receipt of a Redis event to a successful live WebSocket write.",
			Buckets: latencyBuckets,
		}),
		OutboxBatchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "jacko_outbox_batch_duration_seconds",
			Help:    "Time spent publishing a nonempty outbox batch and committing its status, or on a failed attempt.",
			Buckets: latencyBuckets,
		}, []string{"result"}),
		OutboxPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jacko_outbox_events_published_total",
			Help: "Outbox events successfully marked as published by this worker.",
		}),
		OutboxFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "jacko_outbox_publish_failures_total",
			Help: "Failed outbox batch attempts by this worker.",
		}),
		OutboxOldestPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jacko_outbox_oldest_pending_seconds",
			Help: "Age of the oldest unpublished event in the shared database, or zero when empty.",
		}),
		OutboxProbeOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "jacko_outbox_probe_ok",
			Help: "Whether the most recent backlog sample succeeded.",
		}),
	}
	m.registry.MustRegister(
		m.httpDuration, m.WSConnections, m.WSFrames, m.WSQueueOverflows, m.WSResyncs, m.WSLiveDelivery,
		m.OutboxBatchDuration, m.OutboxPublished, m.OutboxFailures, m.OutboxOldestPending, m.OutboxProbeOK,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)
	return m
}

func (m *Metrics) HTTP(route string, handler http.Handler) http.Handler {
	return promhttp.InstrumentHandlerDuration(m.httpDuration.MustCurryWith(prometheus.Labels{"route": route}), handler)
}

func (m *Metrics) ObserveQueueDepth(queued func() float64) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "jacko_ws_queued_events",
		Help: "Total number of live events currently queued across local subscribers.",
	}, queued))
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		MaxRequestsInFlight: 2,
	})
}
