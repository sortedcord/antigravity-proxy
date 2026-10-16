// Package lifecycle provides terminal, concurrent-safe resource cleanup.
package lifecycle

import (
	liblife "github.com/terastal-hq/proxylib/lifecycle"
)

// Closer runs cleanup once. Concurrent callers join the same cleanup and receive
// its error.
type Closer = liblife.Closer
