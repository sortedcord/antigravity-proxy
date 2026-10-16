package atomicfile

import (
	"github.com/terastal-hq/proxylib/privatefile"
)

// Replace requests durable metadata as Windows cannot fsync a directory.
// Both source and target handles must be closed before calling it.
func Replace(from, to string) error {
	return privatefile.Replace(from, to)
}

func SyncDir(string) error { return nil } // Replace supplies the durability boundary.
