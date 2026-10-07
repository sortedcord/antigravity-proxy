//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !solaris && !aix && !windows

package status

import (
	"fmt"
	"os"
	"runtime"
)

func unsupportedHistoryStorage() error {
	return fmt.Errorf("quota history storage is unsupported on %s: exclusive locking and durable replacement are required", runtime.GOOS)
}

func lockHistoryFile(*os.File) error {
	return unsupportedHistoryStorage()
}

func replaceHistoryFile(string, string) error {
	return unsupportedHistoryStorage()
}

func syncHistoryDirectory(string) error {
	return unsupportedHistoryStorage()
}
