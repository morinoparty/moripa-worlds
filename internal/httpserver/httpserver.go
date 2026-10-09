// Package httpserver は /healthz・/readyz・/metrics を提供する。
package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ReadyCheck は準備ができていなければエラーを返す。
type ReadyCheck func(ctx context.Context) error

// New は HTTP ハンドラーを作る。呼び出し側は返った ServeMux に API などを足してよい。
func New(reg *prometheus.Registry, checks map[string]ReadyCheck) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		failed := false
		var body []byte
		for name, check := range checks {
			if err := check(ctx); err != nil {
				failed = true
				body = append(body, name+": "+err.Error()+"\n"...)
			} else {
				body = append(body, name+": ok\n"...)
			}
		}
		if failed {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write(body)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	return mux
}
