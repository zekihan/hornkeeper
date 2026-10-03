package controller

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	Reconciles *prometheus.CounterVec
	Fields     *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hornkeeper_reconciliations_total",
			Help: "PVC reconciliations by result (ignored, enabled, error).",
		}, []string{"result"}),
		Fields: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hornkeeper_field_reconciliations_total",
			Help: "Field reconciliations by field and result (patched, unchanged, invalid, waiting, error).",
		}, []string{"field", "result"}),
	}
	reg.MustRegister(m.Reconciles, m.Fields)
	return m
}
