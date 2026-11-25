package probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRun(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch r.URL.Path {
		case "/healthz":
			sha := "0123456789abcdef"
			if n == 1 {
				sha = "fedcba9876543210" // an old task still behind the load balancer
			}
			_, _ = io.WriteString(w, `{"build": {"sha": "`+sha+`"}}`)
		case "/keys":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer x" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "unknown key")
		}
	}))
	defer srv.Close()
	p := &Prober{HTTP: srv.Client(), Log: t.Logf}
	ctx := context.Background()

	if err := p.Run(ctx, Check{URL: srv.URL + "/healthz", Revision: ".build.sha", Attempts: 5, Streak: 2}, "0123456"); err != nil {
		t.Errorf("health: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("health took %d requests, want 3 (one stale, two in a row)", calls.Load())
	}
	if err := p.Run(ctx, Check{URL: srv.URL + "/healthz", Revision: ".build.sha", Attempts: 2}, "aaaaaaaa"); err == nil {
		t.Error("health of the wrong commit passed")
	}
	smoke := Check{URL: srv.URL + "/keys", Method: http.MethodPost, Headers: map[string]string{"Authorization": "Bearer x"},
		Body: "{}", Expect: 401, Contains: "unknown key"}
	if err := p.Run(ctx, smoke, ""); err != nil {
		t.Errorf("smoke: %v", err)
	}
	smoke.Contains = "welcome"
	if err := p.Run(ctx, smoke, ""); err == nil {
		t.Error("smoke without the expected text passed")
	}
}

func TestSameCommit(t *testing.T) {
	if !SameCommit("0123456", "0123456789") || SameCommit("012345", "0123456789") || SameCommit("0123457", "0123456789") {
		t.Error("SameCommit")
	}
}
