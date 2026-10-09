package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/morinoparty/moripa-worlds/internal/metrics"
)

func TestEndpoints(t *testing.T) {
	m := metrics.New(func() float64 { return 3 })
	m.Connection("status", "created")
	m.Reaped("idle")
	m.Created()
	m.Swept()
	ok := true
	h := New(m.Registry, map[string]ReadyCheck{
		"s3": func(context.Context) error {
			if ok {
				return nil
			}
			return errors.New("unreachable")
		},
	})
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Errorf("/healthz = %d", code)
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Errorf("/readyz = %d", code)
	}
	ok = false
	if code, body := get("/readyz"); code != 503 || !strings.Contains(body, "s3: unreachable") {
		t.Errorf("/readyz = %d %q", code, body)
	}
	code, body := get("/metrics")
	for _, want := range []string{
		"worlds_running 3",
		`waker_connections_total{result="created",state="status"} 1`,
		"worlds_created_total 1",
		`worlds_reaped_total{reason="idle"} 1`,
		"schematics_swept_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics (%d) missing %q", code, want)
		}
	}
}
