package atomicfile

import (
	"syscall"
	"unsafe"
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

// Replace requests durable metadata as Windows cannot fsync a directory.
// Both source and target handles must be closed before calling it.
func Replace(from, to string) error {
	source, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	// MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH.
	result, _, err := moveFileEx.Call(uintptr(unsafe.Pointer(source)), uintptr(unsafe.Pointer(target)), 0x1|0x8)
	if result == 0 {
		return err
	}
	return nil
}

func SyncDir(string) error { return nil } // Replace supplies the durability boundary.
