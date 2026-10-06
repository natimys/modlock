//go:build !windows

package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireSyncLock(root string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(root, ".modlock-sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another ModLock sync is already running")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
