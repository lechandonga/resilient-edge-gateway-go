package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestForward_RelaysStatusHeaderBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("teapot-body"))
	}))
	defer up.Close()

	p := New(up.Client())
	req := httptest.NewRequest("GET", "/anything", nil)
	rec := httptest.NewRecorder()
	if err := p.Forward(context.Background(), rec, req, up.Listener.Addr().String()); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if rec.Code != http.StatusTeapot || rec.Header().Get("X-Upstream") != "yes" || rec.Body.String() != "teapot-body" {
		t.Fatalf("relay wrong: code=%d header=%q body=%q", rec.Code, rec.Header().Get("X-Upstream"), rec.Body.String())
	}
	t.Logf("relayed status=%d header and body from %s", rec.Code, up.Listener.Addr())
}

func TestForward_ClassifiesUnreachable(t *testing.T) {
	// Nothing listens on this address (reserve then release).
	ln := newClosedListener(t)
	p := New(http.DefaultClient)
	req := httptest.NewRequest("GET", "/x", nil)
	err := p.Forward(context.Background(), httptest.NewRecorder(), req, ln)
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindUnreachable {
		t.Fatalf("expected KindUnreachable, got %v", err)
	}
	t.Logf("connection refused classified as %s", pe.Kind)
}

func TestForward_ClassifiesTimeout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer up.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p := New(up.Client())
	req := httptest.NewRequest("GET", "/slow", nil)
	err := p.Forward(ctx, httptest.NewRecorder(), req, up.Listener.Addr().String())
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindTimeout {
		t.Fatalf("expected KindTimeout, got %v", err)
	}
	t.Logf("deadline exceeded classified as %s", pe.Kind)
}
