package diff

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"modlock/internal/lockfile"
)

type Result struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Updated   []Update `json:"updated"`
	Unchanged int      `json:"unchanged"`
}

type Update struct {
	Old lockfile.ModEntry `json:"old"`
	New lockfile.ModEntry `json:"new"`
}

func Locks(old, next *lockfile.File) Result {
	a, b := map[string]lockfile.ModEntry{}, map[string]lockfile.ModEntry{}
	for _, m := range old.Mods {
		a[m.Identity()] = m
	}
	for _, m := range next.Mods {
		b[m.Identity()] = m
	}
	var r Result
	for id, n := range b {
		o, ok := a[id]
		if !ok {
			r.Added = append(r.Added, n.Filename)
			continue
		}
		if o.Filename != n.Filename || o.DisplayVersion() != n.DisplayVersion() || o.URL != n.URL || o.Path != n.Path {
			r.Updated = append(r.Updated, Update{Old: o, New: n})
		} else {
			r.Unchanged++
		}
	}
	for id, o := range a {
		if _, ok := b[id]; !ok {
			r.Removed = append(r.Removed, o.Filename)
		}
	}
	sort.Strings(r.Added)
	sort.Strings(r.Removed)
	sort.Slice(r.Updated, func(i, j int) bool { return r.Updated[i].New.Identity() < r.Updated[j].New.Identity() })
	return r
}

func Local(modsDir string, lock *lockfile.File) (Result, error) {
	a, b := map[string]bool{}, map[string]bool{}
	for _, m := range lock.Mods {
		a[m.Filename] = true
	}
	entries, err := os.ReadDir(modsDir)
	if os.IsNotExist(err) {
		return compare(a, b), nil
	}
	if err != nil {
		return Result{}, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".jar") {
			if _, err := lockfile.ResolveWithin(modsDir, e.Name()); err != nil {
				return Result{}, err
			}
			b[e.Name()] = true
		}
	}
	return compare(a, b), nil
}

func compare(old, next map[string]bool) Result {
	var r Result
	for x := range next {
		if !old[x] {
			r.Added = append(r.Added, x)
		} else {
			r.Unchanged++
		}
	}
	for x := range old {
		if !next[x] {
			r.Removed = append(r.Removed, x)
		}
	}
	sort.Strings(r.Added)
	sort.Strings(r.Removed)
	return r
}
