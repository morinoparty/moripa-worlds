package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/world"
)

const (
	token   = "test-token-0123456789"
	idOK    = "0199c8a0-7b3e-7f12-9a4c-1d2e3f405162"
	idNone  = "0199c8a0-7b3e-7f12-9a4c-000000000000"
	idOld   = "0199c8a0-7b3e-7f12-9a4c-000000000001"
	idS3Err = "0199c8a0-7b3e-7f12-9a4c-000000000002"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fakeStore struct{}

func (fakeStore) Get(_ context.Context, id string) (*schematic.Info, error) {
	title := "テスト"
	past := now.Add(-time.Hour)
	switch id {
	case idOK:
		return &schematic.Info{ID: id, Title: &title}, nil
	case idOld:
		return &schematic.Info{ID: id, Title: &title, DeleteAt: &past}, nil
	case idS3Err:
		return nil, errors.New("connection refused")
	}
	return nil, schematic.ErrNotFound
}

type fakeWorlds struct {
	mu            sync.Mutex
	exists, ready bool
	result        world.EnsureResult
	err           error
	ensured       int
}

func (f *fakeWorlds) Status(string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exists, f.ready
}

func (f *fakeWorlds) Ensure(context.Context, *schematic.Info) (world.EnsureResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured++
	return f.result, f.err
}

func newServer(w *fakeWorlds, ping Pinger) *Server {
	return &Server{
		Token:   token,
		Store:   fakeStore{},
		Worlds:  w,
		Address: func(id string) string { return "w-" + id + ".worlds.svc.cluster.local:25565" },
		Ping:    ping,
		Now:     func() time.Time { return now },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func do(t *testing.T, h http.Handler, method, path, auth string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestAuth(t *testing.T) {
	h := newServer(&fakeWorlds{}, nil).Handler()
	for _, auth := range []string{"", "Bearer", "Bearer ", "Bearer wrong", "Basic " + token, "bearer " + token, "Bearer " + token + "x"} {
		for _, m := range []struct{ method, path string }{
			{http.MethodGet, "/v1/worlds/" + idOK},
			{http.MethodPost, "/v1/worlds/" + idOK + "/ensure"},
		} {
			code, body := do(t, h, m.method, m.path, auth)
			if code != http.StatusUnauthorized || !strings.Contains(body, "unauthorized") {
				t.Errorf("%s %s with %q = %d %s", m.method, m.path, auth, code, body)
			}
		}
	}
	if code, _ := do(t, h, http.MethodGet, "/v1/worlds/"+idOK, "Bearer "+token); code != http.StatusOK {
		t.Errorf("valid token = %d", code)
	}
	// トークンが空のサーバーは何も通さない。
	s := newServer(&fakeWorlds{}, nil)
	s.Token = ""
	if code, _ := do(t, s.Handler(), http.MethodGet, "/v1/worlds/"+idOK, "Bearer "); code != http.StatusUnauthorized {
		t.Errorf("empty server token = %d", code)
	}
}

func TestInvalidID(t *testing.T) {
	h := newServer(&fakeWorlds{}, nil).Handler()
	for _, id := range []string{"abc", "0199C8A0-7B3E-7F12-9A4C-1D2E3F405162", "069a79f4-44e9-4726-a5be-fca90e38aaf5"} {
		if code, _ := do(t, h, http.MethodGet, "/v1/worlds/"+id, "Bearer "+token); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d", id, code)
		}
	}
	if code, _ := do(t, h, http.MethodDelete, "/v1/worlds/"+idOK, "Bearer "+token); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d", code)
	}
}

// client は Server を httptest で立て、apiclient で叩く (配線ごと確かめる)。
func client(t *testing.T, s *Server) *apiclient.Client {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return apiclient.New(ts.URL, token)
}

func TestStateMapping(t *testing.T) {
	pingErr := errors.New("refused")
	tests := []struct {
		name          string
		id            string
		exists, ready bool
		ping          error
		wantFound     bool
		wantState     apiclient.State
		wantTitle     bool
	}{
		{"absent", idOK, false, false, nil, true, apiclient.StateAbsent, true},
		{"starting", idOK, true, false, nil, true, apiclient.StateStarting, true},
		{"ready", idOK, true, true, nil, true, apiclient.StateReady, true},
		{"ready but ping fails", idOK, true, true, pingErr, true, apiclient.StateStarting, true},
		{"not found", idNone, true, true, nil, false, apiclient.StateAbsent, false},
		{"expired", idOld, true, true, nil, false, apiclient.StateAbsent, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(&fakeWorlds{exists: tt.exists, ready: tt.ready}, func(context.Context, string) error { return tt.ping })
			w, err := client(t, s).State(context.Background(), tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if w.Found != tt.wantFound || w.State != tt.wantState || (w.Title != nil) != tt.wantTitle {
				t.Fatalf("got %+v", w)
			}
			if w.Address != "w-"+tt.id+".worlds.svc.cluster.local:25565" || w.Result != "" {
				t.Fatalf("got %+v", w)
			}
			if tt.wantTitle && w.DisplayTitle() != "テスト" {
				t.Fatalf("title = %q", w.DisplayTitle())
			}
		})
	}
}

func TestStateStoreError(t *testing.T) {
	_, err := client(t, newServer(&fakeWorlds{}, nil)).State(context.Background(), idS3Err)
	var se *apiclient.ErrStatus
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureMapping(t *testing.T) {
	tests := []struct {
		name          string
		id            string
		exists, ready bool
		result        world.EnsureResult
		err           error
		want          apiclient.Result
		wantState     apiclient.State
		wantEnsure    int
	}{
		{"created", idOK, false, false, world.Created, nil, apiclient.ResultCreated, apiclient.StateStarting, 1},
		{"existing starting", idOK, true, false, world.Existing, nil, apiclient.ResultExisting, apiclient.StateStarting, 1},
		{"existing ready", idOK, true, true, world.Existing, nil, apiclient.ResultExisting, apiclient.StateReady, 1},
		{"full", idOK, false, false, world.Full, nil, apiclient.ResultFull, apiclient.StateAbsent, 1},
		{"rate limited", idOK, false, false, world.RateLimited, nil, apiclient.ResultRateLimited, apiclient.StateAbsent, 1},
		{"k8s error", idOK, false, false, 0, errors.New("boom"), apiclient.ResultError, apiclient.StateAbsent, 1},
		{"not found", idNone, false, false, world.Created, nil, apiclient.ResultNotFound, apiclient.StateAbsent, 0},
		{"expired", idOld, false, false, world.Created, nil, apiclient.ResultNotFound, apiclient.StateAbsent, 0},
		{"s3 error", idS3Err, false, false, world.Created, nil, apiclient.ResultError, apiclient.StateAbsent, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := &fakeWorlds{exists: tt.exists, ready: tt.ready, result: tt.result, err: tt.err}
			w, err := client(t, newServer(fw, nil)).Ensure(context.Background(), tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if w.Result != tt.want || w.State != tt.wantState || fw.ensured != tt.wantEnsure {
				t.Fatalf("got %+v (ensured %d)", w, fw.ensured)
			}
		})
	}
}

func TestPingCached(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	s := newServer(&fakeWorlds{exists: true, ready: true}, func(context.Context, string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	})
	c := client(t, s)
	for range 5 {
		if w, err := c.State(context.Background(), idOK); err != nil || w.State != apiclient.StateReady {
			t.Fatalf("State = %+v, %v", w, err)
		}
	}
	if calls != 1 {
		t.Fatalf("ping calls = %d, want 1 (cached)", calls)
	}
}

func TestClientRejectsWrongToken(t *testing.T) {
	ts := httptest.NewServer(newServer(&fakeWorlds{}, nil).Handler())
	defer ts.Close()
	_, err := apiclient.New(ts.URL, "wrong").State(context.Background(), idOK)
	var se *apiclient.ErrStatus
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("error leaks the token: %v", err)
	}
}
