package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"modlock/internal/atomicfile"
	"modlock/internal/failure"
	"modlock/internal/lockfile"
	syncer "modlock/internal/sync"
)

type addModRequest struct {
	Mod         lockfile.ModEntry  `json:"mod"`
	Targets     []string           `json:"targets"`
	StagedFile  string             `json:"staged_file"`
	TargetRoots syncer.TargetRoots `json:"target_roots"`
}

func addAuthorMod(root string, p addModRequest) (any, error) {
	if len(p.Targets) == 0 {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("targets must not be empty"))
	}
	stage := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p.StagedFile)))
	parts := strings.Split(stage, "/")
	if len(parts) != 3 || parts[0] != ".modlock" || parts[1] != "staging" || parts[2] == "" || parts[2] == "." || parts[2] == ".." {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("staged_file must be a single file inside .modlock/staging"))
	}
	if err := lockfile.ValidateRelative(parts[2]); err != nil || strings.ContainsAny(parts[2], `/\`) || filepath.Base(parts[2]) != parts[2] {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("staged_file must have a safe single filename"))
	}
	stagePath, err := lockfile.ResolveWithin(root, stage)
	if err != nil {
		return nil, failure.Wrap(failure.InvalidRequest, err)
	}
	info, err := os.Lstat(stagePath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, failure.Wrap(failure.Conflict, fmt.Errorf("staged artifact is not a regular file"))
	}
	b, err := os.ReadFile(stagePath)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	digest := hex.EncodeToString(h[:])
	if p.Mod.SHA256 != "" && !strings.EqualFold(p.Mod.SHA256, digest) {
		return nil, failure.WrapDetails(failure.Conflict, fmt.Errorf("staged artifact hash does not match provider metadata"), map[string]any{"kind": "changed_during_apply", "sha256": digest})
	}
	p.Mod.SHA256 = digest
	p.Mod.Targets = append([]string(nil), p.Targets...)
	path, old, err := authorLock(root)
	if err != nil {
		return nil, err
	}
	if old.Schema != 3 {
		return nil, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("add-mod requires schema 3"))
	}
	next := *old
	next.Mods = append([]lockfile.ModEntry(nil), old.Mods...)
	for _, m := range next.Mods {
		for _, id := range p.Targets {
			for _, existing := range m.Targets {
				if existing == id && strings.EqualFold(m.Filename, p.Mod.Filename) {
					return nil, failure.WrapDetails(failure.Conflict, fmt.Errorf("a mod with filename %q is already managed for target %q", p.Mod.Filename, id), map[string]any{"kind": "existing_unmanaged", "target_id": id, "filename": p.Mod.Filename})
				}
			}
		}
		if m.Identity() == p.Mod.Identity() {
			return nil, failure.Wrap(failure.Conflict, fmt.Errorf("mod %q is already managed", p.Mod.Identity()))
		}
	}
	next.Mods = append(next.Mods, p.Mod)
	if err := next.Validate(); err != nil {
		return nil, failure.Wrap(failure.InvalidRequest, err)
	}
	if err := syncer.ValidateTargetRoots(root, &next, p.TargetRoots); err != nil {
		return nil, err
	}
	modsRel := filepath.ToSlash(filepath.Join(next.Pack.ModsDir, p.Mod.Filename))
	type saved struct {
		path   string
		data   []byte
		exists bool
	}
	var backups []saved
	rollback := func() {
		for i := len(backups) - 1; i >= 0; i-- {
			v := backups[i]
			if v.exists {
				_ = atomicfile.Write(v.path, v.data, 0644)
			} else {
				_ = os.Remove(v.path)
			}
		}
	}
	for _, id := range p.Targets {
		dst, err := lockfile.ResolveWithin(p.TargetRoots[id], modsRel)
		if err != nil {
			rollback()
			return nil, failure.Wrap(failure.Conflict, err)
		}
		if current, readErr := os.ReadFile(dst); readErr == nil {
			cur := sha256.Sum256(current)
			if hex.EncodeToString(cur[:]) != digest {
				rollback()
				return nil, failure.WrapDetails(failure.Conflict, fmt.Errorf("destination already contains different data: %s", modsRel), map[string]any{"kind": "existing_unmanaged", "target_id": id, "target": modsRel})
			}
			backups = append(backups, saved{path: dst, data: current, exists: true})
			continue
		} else if !os.IsNotExist(readErr) {
			rollback()
			return nil, readErr
		}
		backups = append(backups, saved{path: dst})
		if err := atomicfile.Write(dst, b, 0644); err != nil {
			rollback()
			return nil, err
		}
	}
	if err := lockfile.Write(path, &next); err != nil {
		rollback()
		return nil, err
	}
	return map[string]any{"lock": &next, "sha256": digest}, nil
}
