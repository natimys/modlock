//go:build !windows

package sync

import (
	"fmt"
	"os"
	"syscall"

	"modlock/internal/lockfile"
)

func acquireSyncLock(root string) (func(), error) {
	path, err := lockfile.ResolveWithin(root, ".modlock-sync.lock")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another ModLock sync is already running")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
