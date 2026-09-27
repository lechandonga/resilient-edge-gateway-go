package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Kind classifies a forwarding failure so callers can return distinct
// status codes and clients can tell failure causes apart.
type Kind int

const (
	// KindTimeout means the backend did not answer before the deadline.
	KindTimeout Kind = iota
	// KindUnreachable means the backend could not be connected to.
	KindUnreachable
	// KindBadResponse means the backend reply could not be relayed.
	KindBadResponse
)

func (k Kind) String() string {
	switch k {
	case KindTimeout:
		return "timeout"
	case KindUnreachable:
		return "unreachable"
	case KindBadResponse:
		return "bad_response"
	default:
		return "unknown"
	}
}

// Error is the classified forwarding error.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s: %v", e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// hopByHopHeaders are connection-specific headers that must not be
// forwarded (per RFC 7230).
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Proxy forwards HTTP requests to backend instances.
type Proxy struct {
	client *http.Client
}

// New builds a Proxy. The client should have no global Timeout so that
// per-request context deadlines govern timing; its transport governs
// connection reuse and bounds idle connections.
func New(client *http.Client) *Proxy {
	return &Proxy{client: client}
}

// Do performs the upstream round trip and returns the backend response for
// the caller to relay. Receiving any HTTP response (even a 5xx) is a
// successful round trip for health purposes; only transport-level failure
// yields a classified error. The caller must close resp.Body.
func (p *Proxy) Do(ctx context.Context, req *http.Request, addr string) (*http.Response, error) {
	outReq := req.Clone(ctx)
	outReq.RequestURI = ""
	outReq.URL.Scheme = "http"
	outReq.URL.Host = addr
	outReq.Host = addr
	outReq.Close = false
	for _, h := range hopByHopHeaders {
		outReq.Header.Del(h)
	}

	resp, err := p.client.Do(outReq)
	if err != nil {
		return nil, classify(err)
	}
	return resp, nil
}

// Forward sends req to addr and streams the response into w.
func (p *Proxy) Forward(ctx context.Context, w http.ResponseWriter, req *http.Request, addr string) error {
	resp, err := p.Do(ctx, req, addr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return WriteResponse(w, resp)
}

// WriteResponse relays an upstream response's status, headers and body.
func WriteResponse(w http.ResponseWriter, resp *http.Response) error {
	for _, h := range hopByHopHeaders {
		resp.Header.Del(h)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err := io.Copy(w, resp.Body)
	return err
}

// classify maps a transport error to a Kind: deadline failure -> timeout,
// everything else (connection refused/reset/DNS) -> unreachable.
func classify(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: KindTimeout, Err: err}
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		return &Error{Kind: KindTimeout, Err: err}
	}
	return &Error{Kind: KindUnreachable, Err: err}
}
