package proxy

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics はプロキシのメトリクス。nil のまま使ってもよい。
type Metrics struct {
	Registry      *prometheus.Registry
	WorldConnects *prometheus.CounterVec
	Logins        *prometheus.CounterVec
}

// NewMetrics はメトリクスを作って新しいレジストリに登録する。
// waiting は players_waiting の現在値を返す関数。
func NewMetrics(waiting func() float64) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		WorldConnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "world_connects_total",
			Help: "ロビーからワールドへの移動の試み (result=success|failed|gave_up|timeout|not_found|busy|error)。",
		}, []string{"result"}),
		Logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "logins_total",
			Help: "ログインの振り分け (result=world|lobby|disconnect|denied_host|denied_version)。",
		}, []string{"result"}),
	}
	if waiting == nil {
		waiting = func() float64 { return 0 }
	}
	reg.MustRegister(
		m.WorldConnects, m.Logins,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "players_waiting",
			Help: "ロビーでワールドの起動を待っているプレイヤーの数。",
		}, waiting),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

func (m *Metrics) connect(result string) {
	if m == nil {
		return
	}
	m.WorldConnects.WithLabelValues(result).Inc()
}

func (m *Metrics) login(result string) {
	if m == nil {
		return
	}
	m.Logins.WithLabelValues(result).Inc()
}
