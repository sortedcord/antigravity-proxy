//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris || aix

package status

import "os"

func replaceHistoryFile(from, to string) error {
	return os.Rename(from, to)
}

func syncHistoryDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
