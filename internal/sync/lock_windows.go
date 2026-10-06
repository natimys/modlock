//go:build windows

package sync

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"modlock/internal/failure"
	"modlock/internal/lockfile"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
var unlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")

func acquireSyncLock(root string) (func(), error) {
	path, err := lockfile.ResolveWithin(root, ".modlock-sync.lock")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var ov syscall.Overlapped
	const exclusive = 0x2
	const failImmediately = 0x1
	r, _, _ := lockFileEx.Call(f.Fd(), exclusive|failImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
	if r == 0 {
		_ = f.Close()
		return nil, failure.Wrap(failure.Busy, fmt.Errorf("another ModLock sync is already running"))
	}
	return func() { unlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov))); _ = f.Close() }, nil
}
