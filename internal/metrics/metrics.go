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
	WakerConnections  *prometheus.CounterVec
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
		WakerConnections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "waker_connections_total",
			Help: "Gate から転送されてきた接続の数 (state=status|login|unknown, result=starting|running|not_found|busy|error|bad_request)。",
		}, []string{"state", "result"}),
		WorldsCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "worlds_created_total",
			Help: "作成したワールド (Deployment) の数。",
		}),
		WorldsReaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "worlds_reaped_total",
			Help: "削除したワールドの数 (reason=idle|startup_timeout|schematic_missing|expired|swept)。",
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
		m.WakerConnections, m.WorldsCreated, m.WorldsReaped, m.SchematicsSwept, m.worldsRunningFunc,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Connection は waker_connections_total を 1 増やす。
func (m *Metrics) Connection(state, result string) {
	if m == nil {
		return
	}
	m.WakerConnections.WithLabelValues(state, result).Inc()
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
