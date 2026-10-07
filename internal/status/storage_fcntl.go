//go:build aix || (solaris && !illumos)

package status

import (
	"errors"
	"io"
	"os"
	"syscall"
)

func lockHistoryFile(file *os.File) error {
	// A zero length covers the remainder of the file. F_SETLK is nonblocking;
	// a live owner fails acquisition instead of indefinitely delaying startup.
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: 0, Len: 0}
	for {
		err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
