// Package lifecycle provides terminal, concurrent-safe resource cleanup.
package lifecycle

import "sync"

// Closer runs cleanup once. Concurrent callers join the same cleanup and receive
// its error. Owners must gate admission under their own mutex before joining
// workers; Closer is not a substitute for that lifecycle contract.
// A Closer must not be copied after first use.
type Closer struct {
	once sync.Once
	err  error
}

func (c *Closer) Close(cleanup func() error) error {
	c.once.Do(func() { c.err = cleanup() })
	return c.err
}
