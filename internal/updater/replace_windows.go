//go:build windows

package updater

import (
	"syscall"
	"unsafe"
)

func replaceFile(src, dst string) error {
	from, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	const movefileReplaceExisting = 0x1
	const movefileWriteThrough = 0x8
	r1, _, callErr := syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), movefileReplaceExisting|movefileWriteThrough)
	if r1 == 0 {
		return callErr
	}
	return nil
}
