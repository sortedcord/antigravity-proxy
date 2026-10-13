package proxy

import (
	"context"
	"sync"
)

// sharedFlight publishes one result to independently cancelable waiters. The
// owning cache mutex protects waiter counts and publication; closing done makes
// the immutable result visible to successful waiters. The owner decides whether
// an abandoned or invalidated flight may still populate its cache.
type sharedFlight[T any] struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   T
	err     error
}

func newSharedFlight[T any](cancel context.CancelFunc) *sharedFlight[T] {
	return &sharedFlight[T]{done: make(chan struct{}), cancel: cancel}
}

// join and finish must run under the owning mutex.
func (f *sharedFlight[T]) join() { f.waiters++ }

func (f *sharedFlight[T]) finish(value T, err error) {
	f.value, f.err = value, err
	close(f.done)
}

func (f *sharedFlight[T]) wait(ctx context.Context, mu *sync.Mutex, abandon func()) (T, error) {
	select {
	case <-ctx.Done():
		mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			abandon()
			f.cancel()
		}
		mu.Unlock()
	case <-f.done:
		if ctx.Err() == nil {
			return f.value, f.err
		}
	}
	var zero T
	return zero, ctx.Err()
}
