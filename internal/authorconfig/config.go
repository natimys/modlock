// Package authorconfig stores local publishing preferences separately from the
// public ModLock file list.
package authorconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"modlock/internal/atomicfile"
	"modlock/internal/lockfile"
)

const RelativePath = ".modlock/author.toml"

type Config struct {
	IncludeDirs   []string      `toml:"include_dirs,omitempty" json:"include_dirs,omitempty"`
	ExcludePaths  []string      `toml:"exclude_paths,omitempty" json:"exclude_paths,omitempty"`
	IgnoredModIDs []string      `toml:"ignored_mod_ids,omitempty" json:"ignored_mod_ids,omitempty"`
	TrackedPaths  []TrackedPath `toml:"tracked_paths,omitempty" json:"tracked_paths,omitempty"`
}

type TrackedPath struct {
	Path    string   `toml:"path" json:"path"`
	Targets []string `toml:"targets" json:"targets"`
	Policy  string   `toml:"policy" json:"policy"`
}

func Read(root string) (*Config, error) {
	path, err := lockfile.ResolveWithin(root, RelativePath)
	if err != nil {
		return nil, err
	}
	c := new(Config)
	if _, err = toml.DecodeFile(path, c); err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("read author settings: %w", err)
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func Write(root string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	path, err := lockfile.ResolveWithin(root, RelativePath)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	var b strings.Builder
	if err = toml.NewEncoder(&b).Encode(c); err != nil {
		return err
	}
	return atomicfile.Write(path, []byte(b.String()), 0644)
}

func (c *Config) Validate() error {
	for _, dir := range c.IncludeDirs {
		if err := lockfile.ValidateRelative(dir); err != nil {
			return fmt.Errorf("invalid included directory %q: %w", dir, err)
		}
	}
	for _, pattern := range c.ExcludePaths {
		if strings.Contains(pattern, "\\") || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, ":") || strings.ContainsRune(pattern, 0) {
			return fmt.Errorf("invalid exclusion pattern %q", pattern)
		}
		for _, component := range strings.Split(pattern, "/") {
			if component == ".." {
				return fmt.Errorf("exclusion pattern escapes instance: %q", pattern)
			}
		}
		if _, err := filepath.Match(pattern, "probe"); err != nil {
			return fmt.Errorf("invalid exclusion pattern %q: %w", pattern, err)
		}
	}
	for _, id := range c.IgnoredModIDs {
		if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "\r\n\x00") {
			return fmt.Errorf("invalid ignored mod ID")
		}
	}
	seen := map[string][]string{}
	for i, entry := range c.TrackedPaths {
		if err := lockfile.ValidateRelative(entry.Path); err != nil {
			return fmt.Errorf("invalid tracked path[%d] %q: %w", i, entry.Path, err)
		}
		if reservedAuthorPath(entry.Path) {
			return fmt.Errorf("tracked path[%d] is reserved: %q", i, entry.Path)
		}
		if len(entry.Targets) == 0 {
			return fmt.Errorf("tracked path[%d] must select at least one target", i)
		}
		for _, id := range entry.Targets {
			if err := lockfile.ValidateTargetID(id); err != nil {
				return fmt.Errorf("tracked path[%d]: %w", i, err)
			}
		}
		targetSet := append([]string(nil), entry.Targets...)
		sort.Strings(targetSet)
		for j := 1; j < len(targetSet); j++ {
			if targetSet[j] == targetSet[j-1] {
				return fmt.Errorf("tracked path[%d] has duplicate target %q", i, targetSet[j])
			}
		}
		if entry.Policy != "replace" && entry.Policy != "if_missing" {
			return fmt.Errorf("tracked path[%d]: policy must be replace or if_missing", i)
		}
		key := strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Path))))
		for _, previousTarget := range seen[key] {
			for _, currentTarget := range entry.Targets {
				if previousTarget == currentTarget {
					return fmt.Errorf("duplicate tracked path %q for target %q", entry.Path, currentTarget)
				}
			}
		}
		for previousPath, previousTargets := range seen {
			if pathsOverlap(previousPath, key) {
				for _, previousTarget := range previousTargets {
					for _, currentTarget := range entry.Targets {
						if previousTarget == currentTarget {
							return fmt.Errorf("tracked paths overlap for target %q: %q and %q", currentTarget, previousPath, key)
						}
					}
				}
			}
		}
		for _, include := range c.IncludeDirs {
			includeKey := strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(include))))
			if containsClient(entry.Targets) && pathsOverlap(includeKey, key) {
				return fmt.Errorf("tracked path %q overlaps client include directory %q", entry.Path, include)
			}
		}
		seen[key] = append(seen[key], entry.Targets...)
		sortTargetIDs(entry.Targets)
	}
	sort.Strings(c.IncludeDirs)
	sort.Strings(c.ExcludePaths)
	sort.Strings(c.IgnoredModIDs)
	return nil
}

func containsClient(ids []string) bool {
	for _, id := range ids {
		if id == "client" {
			return true
		}
	}
	return false
}
func pathsOverlap(a, b string) bool {
	return a == "." || b == "." || a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}

func reservedAuthorPath(path string) bool {
	for _, part := range strings.Split(strings.ToLower(filepath.ToSlash(path)), "/") {
		if part == ".modlock" || part == ".git" {
			return true
		}
	}
	return false
}

func (c *Config) ContainsIgnored(id string) bool {
	for _, current := range c.IgnoredModIDs {
		if current == id {
			return true
		}
	}
	return false
}

func sortTargetIDs(ids []string) {
	priority := map[string]int{"client": 0, "server": 1}
	sort.Slice(ids, func(i, j int) bool {
		pi, a := priority[ids[i]]
		pj, b := priority[ids[j]]
		if a != b {
			return a
		}
		if a {
			return pi < pj
		}
		return ids[i] < ids[j]
	})
}
