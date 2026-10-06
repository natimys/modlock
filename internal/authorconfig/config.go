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
	IncludeDirs   []string `toml:"include_dirs,omitempty" json:"include_dirs,omitempty"`
	ExcludePaths  []string `toml:"exclude_paths,omitempty" json:"exclude_paths,omitempty"`
	IgnoredModIDs []string `toml:"ignored_mod_ids,omitempty" json:"ignored_mod_ids,omitempty"`
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
	sort.Strings(c.IncludeDirs)
	sort.Strings(c.ExcludePaths)
	sort.Strings(c.IgnoredModIDs)
	return nil
}
