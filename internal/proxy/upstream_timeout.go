package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// The pre-header deadline includes dialing and request upload. Body inactivity
// resets on each read; finite RPCs additionally carry their total context deadline.
func newUpstreamClient(headerTimeout, idleTimeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = headerTimeout
	return &http.Client{Transport: &idleResponseTransport{base: transport, headerTimeout: headerTimeout, timeout: idleTimeout}}
}

type idleResponseTransport struct {
	base          http.RoundTripper
	headerTimeout time.Duration
	timeout       time.Duration
}

// Preserve http.Client.CloseIdleConnections through the transport wrapper.
func (t *idleResponseTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *idleResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	body := &idleResponseBody{parent: req.Context(), cancel: cancel, timeout: t.timeout}
	body.mu.Lock()
	body.deadline = time.Now().Add(t.headerTimeout)
	body.timer = time.AfterFunc(t.headerTimeout, body.expire)
	body.mu.Unlock()
	response, err := t.base.RoundTrip(req.Clone(ctx))
	body.mu.Lock()
	if err != nil || body.expired {
		expired := body.expired
		body.closed = true
		body.timer.Stop()
		cancel()
		body.mu.Unlock()
		if response != nil {
			_ = response.Body.Close()
		}
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if expired {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	// A queued pre-header callback checks this new deadline under the same mutex,
	// so it cannot cancel a response after the handoff to body inactivity succeeds.
	body.body = response.Body
	body.deadline = time.Now().Add(t.timeout)
	body.timer.Reset(t.timeout)
	body.mu.Unlock()
	response.Body = body
	return response, nil
}

type idleResponseBody struct {
	body     io.ReadCloser
	parent   context.Context
	cancel   context.CancelFunc
	timeout  time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
	closed   bool
	expired  bool
}

func (b *idleResponseBody) expire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	// A previously queued timer callback must not cancel after a racing successful
	// read extended the deadline. Timer state and deadline share the same lock.
	if remaining := time.Until(b.deadline); remaining > 0 {
		b.timer.Reset(remaining)
		return
	}
	b.expired = true
	b.closed = true
	b.cancel()
}

func (b *idleResponseBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.mu.Lock()
	if n > 0 && !b.closed {
		b.deadline = time.Now().Add(b.timeout)
		b.timer.Reset(b.timeout)
	}
	if err != nil && !b.closed {
		b.closed = true
		b.timer.Stop()
		b.cancel()
	}
	expired := b.expired
	b.mu.Unlock()
	if err != nil && err != io.EOF {
		if b.parent.Err() != nil {
			return n, b.parent.Err()
		}
		if expired {
			return n, context.DeadlineExceeded
		}
		return n, errors.New("read Antigravity response failed")
	}
	return n, err
}

func (b *idleResponseBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.timer.Stop()
	b.cancel()
	b.mu.Unlock()
	return b.body.Close()
}

// cancelResponseBody transfers a request's context lifetime to its consumer.
// EOF, read failure and Close all release timers without requiring a second read.
type cancelResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.cancel)
	}
	return n, err
}

func (b *cancelResponseBody) Close() error {
	b.once.Do(b.cancel)
	return b.ReadCloser.Close()
}
