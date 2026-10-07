package status

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	historyKernel32   = syscall.NewLazyDLL("kernel32.dll")
	historyLockFileEx = historyKernel32.NewProc("LockFileEx")
	historyMoveFileEx = historyKernel32.NewProc("MoveFileExW")
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

func replaceHistoryFile(from, to string) error {
	source, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	// MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH. Windows does not
	// support fsync on directories; the replacement itself requests durable
	// metadata and runs only after the replacement file has been flushed.
	result, _, err := historyMoveFileEx.Call(uintptr(unsafe.Pointer(source)), uintptr(unsafe.Pointer(target)), 0x1|0x8)
	if result == 0 {
		return err
	}
	return nil
}

func syncHistoryDirectory(string) error {
	// MoveFileExW supplies the Windows durability boundary above.
	return nil
}
