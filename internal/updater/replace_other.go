//go:build !windows

package updater

import "os"

func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		_ = os.Remove(dst)
		return os.Rename(src, dst)
	}
	return nil
}
