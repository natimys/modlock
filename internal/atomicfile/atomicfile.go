package atomicfile

import (
	"os"
	"path/filepath"
)

// Stage writes a complete temporary file beside its destination.
func Stage(path string, data []byte, mode os.FileMode) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

var replaceAtomic = replace

func Commit(tmp, path string) error { return replaceAtomic(tmp, path) }

func Write(path string, data []byte, mode os.FileMode) error {
	tmp, err := Stage(path, data, mode)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return Commit(tmp, path)
}
