// Package scan inventories installed mods without inferring their publisher.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"modlock/internal/lockfile"
)

type Mod struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	ID       string `json:"id,omitempty"`
	Version  string `json:"version,omitempty"`
	Source   string `json:"source,omitempty"`
	Managed  bool   `json:"managed"`
}

func Mods(root string) ([]Mod, error) {
	modsDirectory := "mods"
	var err error
	lockPath, lockErr := lockfile.FindPath(root)
	known := map[string]lockfile.ModEntry{}
	if lockErr == nil {
		f, readErr := lockfile.Read(lockPath)
		if readErr != nil {
			return nil, readErr
		}
		modsDirectory = f.Pack.ModsDir
		for _, mod := range f.Mods {
			known[strings.ToLower(mod.Filename)] = mod
		}
	} else if !os.IsNotExist(lockErr) {
		return nil, lockErr
	}
	modsDir, err := lockfile.ResolveWithin(root, modsDirectory)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(modsDir)
	if os.IsNotExist(err) {
		return []Mod{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]Mod, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".jar") {
			continue
		}
		path, resolveErr := lockfile.ResolveWithin(modsDir, entry.Name())
		if resolveErr != nil {
			return nil, resolveErr
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil, openErr
		}
		h := sha256.New()
		size, copyErr := io.Copy(h, file)
		closeErr := file.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		mod := Mod{Filename: entry.Name(), SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}
		if old, ok := known[strings.ToLower(entry.Name())]; ok {
			mod.ID, mod.Version, mod.Source, mod.Managed = old.Identity(), old.DisplayVersion(), old.Source, true
		}
		result = append(result, mod)
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Filename) < strings.ToLower(result[j].Filename) })
	return result, nil
}
