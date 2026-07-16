package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDurableBudgetResultClassification(t *testing.T) {
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()

	p := &SessionPool{
		cfg:      Config{BudgetURL: srv.URL},
		budget:   srv.Client(),
		sessions: []*Session{{}},
	}
	if session, reason := p.pick(context.Background(), nil); session == nil || reason != "" {
		t.Fatalf("allowed reservation = (%v, %q)", session, reason)
	}
	status = http.StatusTooManyRequests
	if session, reason := p.pick(context.Background(), nil); session != nil || reason != reasonBudgetExceeded {
		t.Fatalf("exhausted reservation = (%v, %q)", session, reason)
	}
	status = http.StatusServiceUnavailable
	if session, reason := p.pick(context.Background(), nil); session != nil || reason != reasonBudgetBackend {
		t.Fatalf("backend failure = (%v, %q)", session, reason)
	}
}
