package gateway

import (
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// testLogger routes gateway logs into the test output so every decision
// (which backend was hit and why) is visible in `go test -v`.
type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, args ...any) {
	l.t.Helper()
	l.t.Logf(format, args...)
}

// mockBackend is a real TCP backend on a fixed address that can be
// stopped (port closed -> connection refused) and started again on the
// same address to simulate recovery.
type mockBackend struct {
	addr   string
	server *http.Server
	ln     net.Listener
	hits   atomic.Int64
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func newMockBackend(t *testing.T, port int, handler http.HandlerFunc) *mockBackend {
	t.Helper()
	b := &mockBackend{addr: fmt.Sprintf("127.0.0.1:%d", port)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		handler(w, r)
	})
	b.server = &http.Server{Handler: mux}
	b.start(t)
	return b
}

func (b *mockBackend) start(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", b.addr)
	if err != nil {
		t.Fatalf("backend %s listen: %v", b.addr, err)
	}
	b.ln = ln
	go func() { _ = b.server.Serve(ln) }()
	waitReady(t, b.addr)
}

// stop closes the listening socket so new connections are refused.
func (b *mockBackend) stop() {
	if b.ln != nil {
		_ = b.ln.Close()
		b.ln = nil
	}
}

// restart brings the backend back on the same address.
func (b *mockBackend) restart(t *testing.T) {
	t.Helper()
	b.start(t)
}

// sleepHandler responds after a delay, used to trigger timeouts.
func sleepHandler(delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, "slow-response")
	}
}

func echoHandler(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Name", name)
		fmt.Fprintf(w, "hello-from-%s", name)
	}
}

func waitReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %s never became ready", addr)
}

func waitClosed(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %s still accepting connections", addr)
}
