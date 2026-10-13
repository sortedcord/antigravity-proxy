//go:build !windows

// Package atomicfile supplies the platform durability boundaries shared by
// credential replacement and quota-history compaction. Callers own staging,
// permissions, locks and the policy for errors after replacement commits.
package atomicfile

import "os"

// Replace atomically replaces a same-directory target. SyncDir must follow it
// after the staged file has been flushed and closed.
func Replace(from, to string) error { return os.Rename(from, to) }

func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
