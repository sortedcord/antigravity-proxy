package status

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	historyKernel32   = syscall.NewLazyDLL("kernel32.dll")
	historyLockFileEx = historyKernel32.NewProc("LockFileEx")
)

func lockHistoryFile(file *os.File) error {
	var overlapped syscall.Overlapped
	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY. Closing the file
	// releases this byte-range lock, including when the owning process exits.
	result, _, err := historyLockFileEx.Call(file.Fd(), 0x2|0x1, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if result == 0 {
		return err
	}
	return nil
}
