package lockfile

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"modlock/internal/atomicfile"
	"modlock/internal/failure"
)

const DefaultFilename = "mod.lock"
const LegacyFilename = "modlock.lock"

func FindPath(root string) (string, error) {
	for _, name := range []string{DefaultFilename, LegacyFilename} {
		p, err := ResolveWithin(root, name)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

type File struct {
	Schema int           `toml:"schema" json:"schema"`
	Pack   Pack          `toml:"pack" json:"pack"`
	Mods   []ModEntry    `toml:"mods" json:"mods"`
	Files  []ManagedFile `toml:"files,omitempty" json:"files,omitempty"`
}

type Pack struct {
	Repository string      `toml:"repository" json:"repository"`
	Branch     string      `toml:"branch" json:"branch"`
	LockPath   string      `toml:"lock_path" json:"lock_path"`
	ModsDir    string      `toml:"mods_dir" json:"mods_dir"`
	Name       string      `toml:"name,omitempty" json:"name,omitempty"`
	Version    string      `toml:"version,omitempty" json:"version,omitempty"`
	Components []Component `toml:"components,omitempty" json:"components,omitempty"`
}

// Component identifies a game or loader version using profile IDs.
type Component struct {
	ID      string `toml:"id" json:"id"`
	Version string `toml:"version" json:"version"`
}

// ManagedFile maps a repository path to an instance path.
type ManagedFile struct {
	Path   string `toml:"path" json:"path"`
	Target string `toml:"target" json:"target"`
	SHA256 string `toml:"sha256" json:"sha256"`
	Policy string `toml:"policy" json:"policy"`
}

type ModEntry struct {
	ID        string `toml:"id,omitempty" json:"id,omitempty"`
	Version   string `toml:"version,omitempty" json:"version,omitempty"`
	Filename  string `toml:"filename" json:"filename"`
	Source    string `toml:"source" json:"source"`
	ProjectID string `toml:"project_id,omitempty" json:"project_id,omitempty"`
	VersionID string `toml:"version_id,omitempty" json:"version_id,omitempty"`
	ModID     int64  `toml:"mod_id,omitempty" json:"mod_id,omitempty"`
	FileID    int64  `toml:"file_id,omitempty" json:"file_id,omitempty"`
	URL       string `toml:"url,omitempty" json:"url,omitempty"`
	Path      string `toml:"path,omitempty" json:"path,omitempty"`
	SHA256    string `toml:"sha256,omitempty" json:"sha256,omitempty"`
}

// Identity returns a stable key for matching versions of the same mod.
// Provider IDs keep older lock files compatible when the explicit id is absent.
func (m ModEntry) Identity() string {
	if m.ID != "" {
		return m.ID
	}
	switch m.Source {
	case "modrinth":
		if m.ProjectID != "" {
			return "modrinth:" + m.ProjectID
		}
	case "curseforge":
		if m.ModID != 0 {
			return fmt.Sprintf("curseforge:%d", m.ModID)
		}
	}
	return "file:" + strings.ToLower(m.Filename)
}

func (m ModEntry) DisplayVersion() string {
	if m.Version != "" {
		return m.Version
	}
	if m.VersionID != "" {
		return m.VersionID
	}
	if m.FileID != 0 {
		return fmt.Sprintf("%d", m.FileID)
	}
	return m.Filename
}

func Read(path string) (*File, error) {
	var f File
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return nil, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("read lock file: %w", err))
	}
	f.Defaults()
	if err := f.Validate(); err != nil {
		return nil, failure.Wrap(failure.UnsupportedFormat, err)
	}
	return &f, nil
}

func (f *File) Defaults() {
	if f.Schema == 0 {
		f.Schema = 1
	}
	if f.Pack.Branch == "" {
		f.Pack.Branch = "main"
	}
	if f.Pack.LockPath == "" {
		f.Pack.LockPath = DefaultFilename
	}
	if f.Pack.ModsDir == "" {
		f.Pack.ModsDir = "mods"
	}
	for i := range f.Mods {
		m := &f.Mods[i]
		if m.ID == "" {
			switch m.Source {
			case "modrinth":
				if m.ProjectID != "" {
					m.ID = "modrinth:" + m.ProjectID
				}
			case "curseforge":
				if m.ModID != 0 {
					m.ID = fmt.Sprintf("curseforge:%d", m.ModID)
				}
			}
		}
		if m.Version == "" {
			if m.VersionID != "" {
				m.Version = m.VersionID
			} else if m.FileID != 0 {
				m.Version = fmt.Sprintf("%d", m.FileID)
			}
		}
	}
}

func (f *File) Validate() error {
	if f.Schema != 1 && f.Schema != 2 {
		return fmt.Errorf("unsupported lock schema %d", f.Schema)
	}
	if strings.TrimSpace(f.Pack.Repository) == "" {
		return fmt.Errorf("pack.repository is required")
	}
	if err := safeRelative(f.Pack.LockPath); err != nil {
		return fmt.Errorf("invalid pack.lock_path: %w", err)
	}
	if err := safeRelative(f.Pack.ModsDir); err != nil {
		return fmt.Errorf("invalid pack.mods_dir: %w", err)
	}
	if f.Schema == 2 {
		if strings.TrimSpace(f.Pack.Name) == "" || strings.TrimSpace(f.Pack.Version) == "" {
			return fmt.Errorf("schema 2 requires pack.name and pack.version")
		}
		componentIDs := map[string]bool{}
		for i, c := range f.Pack.Components {
			key := strings.ToLower(c.ID)
			if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Version) == "" || componentIDs[key] {
				return fmt.Errorf("components[%d]: id and version must be present and IDs unique", i)
			}
			componentIDs[key] = true
		}
	}
	seen := map[string]bool{}
	identities := map[string]bool{}
	targets := map[string]bool{}
	for i, m := range f.Mods {
		if err := safeRelative(m.Filename); err != nil || strings.ContainsAny(m.Filename, "/\\") {
			return fmt.Errorf("mods[%d]: invalid filename", i)
		}
		if m.Filename == "" || filepath.Base(m.Filename) != m.Filename || strings.ToLower(filepath.Ext(m.Filename)) != ".jar" {
			return fmt.Errorf("mods[%d]: invalid filename", i)
		}
		targets[strings.ToLower(filepath.ToSlash(filepath.Join(f.Pack.ModsDir, m.Filename)))] = true
		if f.Schema == 2 && !validSHA256(m.SHA256) {
			return fmt.Errorf("mods[%d] %s: schema 2 requires a SHA-256 digest", i, m.Filename)
		}
		if seen[strings.ToLower(m.Filename)] {
			return fmt.Errorf("mods[%d]: duplicate filename %q", i, m.Filename)
		}
		seen[strings.ToLower(m.Filename)] = true
		if identities[m.Identity()] {
			return fmt.Errorf("mods[%d]: duplicate mod id %q", i, m.Identity())
		}
		identities[m.Identity()] = true
		switch m.Source {
		case "modrinth", "curseforge":
			if m.URL == "" {
				return fmt.Errorf("mods[%d] %s: url is required", i, m.Filename)
			}
		case "repo":
			if err := safeRelative(m.Path); err != nil {
				return fmt.Errorf("mods[%d] %s: invalid repo path: %w", i, m.Filename, err)
			}
		default:
			return fmt.Errorf("mods[%d] %s: unknown source %q", i, m.Filename, m.Source)
		}
	}
	for i, managed := range f.Files {
		if f.Schema != 2 {
			return fmt.Errorf("files[%d]: managed files require schema 2", i)
		}
		if err := safeRelative(managed.Path); err != nil {
			return fmt.Errorf("files[%d]: invalid repository path: %w", i, err)
		}
		if err := safeRelative(managed.Target); err != nil {
			return fmt.Errorf("files[%d]: invalid destination: %w", i, err)
		}
		if !validSHA256(managed.SHA256) {
			return fmt.Errorf("files[%d]: SHA-256 digest is required", i)
		}
		if managed.Policy != "replace" && managed.Policy != "if_missing" {
			return fmt.Errorf("files[%d]: policy must be replace or if_missing", i)
		}
		target := strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(managed.Target))))
		if targets[target] {
			return fmt.Errorf("files[%d]: destination overlaps another managed file", i)
		}
		for existing := range targets {
			if strings.HasPrefix(existing, target+"/") || strings.HasPrefix(target, existing+"/") {
				return fmt.Errorf("files[%d]: destination overlaps another managed path", i)
			}
		}
		targets[target] = true
	}
	return nil
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func safeRelative(p string) error {
	portable := strings.ReplaceAll(p, "\\", "/")
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(portable, "/") || strings.ContainsAny(portable, ":\x00") {
		return fmt.Errorf("must be a relative path")
	}
	for _, component := range strings.Split(portable, "/") {
		if component == "." || component == ".." {
			continue
		}
		if strings.TrimRight(component, " .") != component {
			return fmt.Errorf("trailing spaces and dots are not allowed")
		}
		name := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		if name == "CON" || name == "PRN" || name == "AUX" || name == "NUL" || (len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) && name[3] >= '1' && name[3] <= '9') {
			return fmt.Errorf("reserved Windows filename")
		}
		for _, ch := range component {
			if ch < 32 || strings.ContainsRune("<>\"|?*", ch) {
				return fmt.Errorf("invalid path character")
			}
		}
	}
	c := filepath.Clean(filepath.FromSlash(portable))
	if c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes project root")
	}
	return nil
}

// ValidateRelative checks an untrusted instance-relative path using the same
// Windows-safe rules used by lock entries and filesystem operations.
func ValidateRelative(p string) error { return safeRelative(p) }

func Write(path string, f *File) error {
	f.Defaults()
	sort.Slice(f.Mods, func(i, j int) bool { return strings.ToLower(f.Mods[i].Filename) < strings.ToLower(f.Mods[j].Filename) })
	if err := f.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	var out bytes.Buffer
	if err := toml.NewEncoder(&out).Encode(f); err != nil {
		return err
	}
	return atomicfile.Write(path, out.Bytes(), 0644)
}

func ResolveWithin(root, rel string) (string, error) {
	if err := safeRelative(rel); err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(strings.ReplaceAll(rel, "\\", "/"))))
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(rootAbs, target)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root")
	}
	// Refuse existing links at every component, including the instance root.
	// Checking the lexical path alone does not protect against junctions.
	current := rootAbs
	parts := append([]string{""}, strings.Split(r, string(filepath.Separator))...)
	for _, part := range parts {
		if part != "" && part != "." {
			current = filepath.Join(current, part)
		}
		linked, linkErr := isLink(current)
		if os.IsNotExist(linkErr) {
			break
		}
		if linkErr != nil {
			return "", linkErr
		}
		if linked {
			return "", fmt.Errorf("symlink or junction is not allowed: %s", current)
		}
	}
	return target, nil
}
