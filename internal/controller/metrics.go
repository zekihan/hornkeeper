package controller

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	Reconciles *prometheus.CounterVec
	Fields     *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	mtr := &Metrics{
		Reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hornkeeper_reconciliations_total",
			Help: "PVC reconciliations by result (ignored, enabled, error).",
		}, []string{"result"}),
		Fields: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hornkeeper_field_reconciliations_total",
			Help: "Field reconciliations by field and result (patched, dry-run, unchanged, invalid, waiting, unmanaged, error).",
		}, []string{"field", "result"}),
	}
	reg.MustRegister(mtr.Reconciles, mtr.Fields)
	return mtr
}
