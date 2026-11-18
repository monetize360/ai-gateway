package guardrails

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type metrics struct {
	evaluations *prometheus.CounterVec
	latency     *prometheus.HistogramVec
}

var (
	metricsOnce sync.Once
	shared      metrics
)

func newMetrics() metrics {
	metricsOnce.Do(func() {
		shared = metrics{
			evaluations: promauto.NewCounterVec(prometheus.CounterOpts{
				Name: "guardrail_evaluations_total",
				Help: "Guardrail evaluations by type, stage, and decision",
			}, []string{"type", "stage", "decision"}),
			latency: promauto.NewHistogramVec(prometheus.HistogramOpts{
				Name:    "guardrail_evaluation_latency_ms",
				Help:    "Guardrail evaluator latency in milliseconds",
				Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
			}, []string{"type"}),
		}
	})
	return shared
}

func (m metrics) record(typeName, stage, decision string, latencyMs int64) {
	if m.evaluations == nil {
		return
	}
	m.evaluations.WithLabelValues(typeName, stage, decision).Inc()
	m.latency.WithLabelValues(typeName).Observe(float64(latencyMs))
}
