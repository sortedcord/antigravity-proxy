//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || illumos

package status

import (
	"errors"
	"os"
	"syscall"
)

func lockHistoryFile(file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
