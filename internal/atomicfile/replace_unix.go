//go:build !windows

package atomicfile

import (
	"os"

	"github.com/terastal-hq/proxylib/privatefile"
)

// Replace atomically replaces a same-directory target.
func Replace(from, to string) error { return os.Rename(from, to) }

// SyncDir flushes directory entries to disk.
func SyncDir(path string) error {
	return privatefile.SyncDir(path)
}
