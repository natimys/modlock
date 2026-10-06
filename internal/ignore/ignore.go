package ignore

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

const Filename = "mod.lock.ignore"

type File struct {
	IDs []string `toml:"ids"`
}

func Read(root string) (*File, error) {
	var f File
	_, err := toml.DecodeFile(filepath.Join(root, Filename), &f)
	if os.IsNotExist(err) {
		return &f, nil
	}
	if err != nil {
		return nil, err
	}
	f.normalize()
	return &f, nil
}

func (f *File) Contains(id string) bool {
	for _, x := range f.IDs {
		if strings.EqualFold(x, id) {
			return true
		}
	}
	return false
}

func (f *File) Add(id string) {
	if !f.Contains(id) {
		f.IDs = append(f.IDs, id)
	}
	f.normalize()
}

func (f *File) normalize() {
	seen := map[string]bool{}
	out := f.IDs[:0]
	for _, id := range f.IDs {
		id = strings.TrimSpace(id)
		key := strings.ToLower(id)
		if id != "" && !seen[key] {
			seen[key] = true
			out = append(out, id)
		}
	}
	f.IDs = out
	sort.Strings(f.IDs)
}

func Write(root string, f *File) error {
	f.normalize()
	path := filepath.Join(root, Filename)
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = toml.NewEncoder(out).Encode(f)
	closeErr := out.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	if err = os.Rename(tmp, path); err != nil {
		os.Remove(path)
		err = os.Rename(tmp, path)
	}
	return err
}
