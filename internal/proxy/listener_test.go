package proxy

import (
	"net"
	"testing"
)

// newClosedListener reserves a free port then closes it, returning an
// address guaranteed to refuse connections for the test.
func newClosedListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
