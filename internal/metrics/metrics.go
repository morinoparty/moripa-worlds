// Package metrics は moripa-worlds の Prometheus メトリクスを定義する。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics は各コンポーネントが更新するメトリクスの集合。
// nil のまま使ってもよい (メソッドは nil レシーバで何もしない)。
type Metrics struct {
	Registry          *prometheus.Registry
	APIRequests       *prometheus.CounterVec
	WorldsCreated     prometheus.Counter
	WorldsReaped      *prometheus.CounterVec
	SchematicsSwept   prometheus.Counter
	worldsRunningFunc prometheus.GaugeFunc
}

// New はメトリクスを作って新しいレジストリに登録する。
// running は worlds_running の現在値を返す関数。
func New(running func() float64) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		APIRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_requests_total",
			Help: "プロキシからの API 呼び出しの数 (op=state|ensure, result=absent|starting|ready|not_found|created|existing|full|rate_limited|error|bad_request|unauthorized)。",
		}, []string{"op", "result"}),
		WorldsCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "worlds_created_total",
			Help: "作成したワールド (Deployment) の数。",
		}),
		WorldsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "worlds_reaped_total",
			Help: "削除したワールドの数 (reason=idle|unjoined|startup_timeout|schematic_missing|expired|swept)。",
		}, []string{"reason"}),
		SchematicsSwept: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "schematics_swept_total",
			Help: "delete_at を過ぎて S3 から削除した schematic の数。",
		}),
	}
	if running == nil {
		running = func() float64 { return 0 }
	}
	m.worldsRunningFunc = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "worlds_running",
		Help: "現在存在するワールド (moripa-worlds 管理の Deployment) の数。",
	}, running)
	reg.MustRegister(
		m.APIRequests, m.WorldsCreated, m.WorldsReaped, m.SchematicsSwept, m.worldsRunningFunc,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// APIRequest は api_requests_total を 1 増やす。
func (m *Metrics) APIRequest(op, result string) {
	if m == nil {
		return
	}
	m.APIRequests.WithLabelValues(op, result).Inc()
}

// Created は worlds_created_total を 1 増やす。
func (m *Metrics) Created() {
	if m == nil {
		return
	}
	m.WorldsCreated.Inc()
}

// Reaped は worlds_reaped_total を 1 増やす。
func (m *Metrics) Reaped(reason string) {
	if m == nil {
		return
	}
	m.WorldsReaped.WithLabelValues(reason).Inc()
}

// Swept は schematics_swept_total を 1 増やす。
func (m *Metrics) Swept() {
	if m == nil {
		return
	}
	m.SchematicsSwept.Inc()
}
