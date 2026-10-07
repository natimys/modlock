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

type removeResourceRequest struct {
	Kind           string             `json:"kind"`
	Identity       string             `json:"identity"`
	Targets        []string           `json:"targets,omitempty"`
	ExpectedSHA256 string             `json:"expected_sha256,omitempty"`
	TargetRoots    syncer.TargetRoots `json:"target_roots"`
}

type removeMutation struct {
	path  string
	bytes []byte
}

func removeAuthorResource(root string, p removeResourceRequest) (any, error) {
	if p.Kind != "mod" && p.Kind != "file" {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("kind must be mod or file"))
	}
	if strings.TrimSpace(p.Identity) == "" {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("identity is required"))
	}
	lockPath, old, err := authorLock(root)
	if err != nil {
		return nil, err
	}
	if old.Schema != 3 {
		return nil, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("remove-resource requires schema 3"))
	}
	if err := syncer.ValidateTargetRoots(root, old, p.TargetRoots); err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range p.Targets {
		if err := lockfile.ValidateTargetID(id); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		if wanted[id] {
			return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("duplicate target %q", id))
		}
		wanted[id] = true
	}
	selected := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if len(wanted) == 0 || wanted[id] {
				out = append(out, id)
			}
		}
		return out
	}
	next := *old
	next.Mods = append([]lockfile.ModEntry(nil), old.Mods...)
	next.Files = append([]lockfile.ManagedFile(nil), old.Files...)
	mutations := []removeMutation{}
	removed := []string{}
	found := false
	checkAndPlan := func(id, rel, expected string) error {
		if p.ExpectedSHA256 != "" && !strings.EqualFold(p.ExpectedSHA256, expected) {
			return failure.WrapDetails(failure.Conflict, fmt.Errorf("resource hash changed since the UI loaded it"), map[string]any{"kind": "changed_during_apply", "target_id": id, "target": rel})
		}
		dst, err := lockfile.ResolveWithin(p.TargetRoots[id], rel)
		if err != nil {
			return failure.WrapDetails(failure.Conflict, err, map[string]any{"kind": "target_not_regular", "target_id": id, "target": rel})
		}
		info, err := os.Lstat(dst)
		if os.IsNotExist(err) {
			removed = append(removed, id)
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return failure.WrapDetails(failure.Conflict, fmt.Errorf("managed target is not a regular file: %s", rel), map[string]any{"kind": "target_not_regular", "target_id": id, "target": rel})
		}
		b, err := os.ReadFile(dst)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		actual := hex.EncodeToString(sum[:])
		if expected != "" && !strings.EqualFold(actual, expected) {
			return failure.WrapDetails(failure.Conflict, fmt.Errorf("managed target was modified locally: %s", rel), map[string]any{"kind": "locally_modified", "target_id": id, "target": rel, "sha256": actual})
		}
		mutations = append(mutations, removeMutation{path: dst, bytes: b})
		removed = append(removed, id)
		return nil
	}
	switch p.Kind {
	case "mod":
		idx := -1
		for i, m := range next.Mods {
			if m.Identity() == p.Identity || m.ID == p.Identity {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("managed mod %q was not found", p.Identity))
		}
		m := next.Mods[idx]
		chosen := selected(m.Targets)
		if len(chosen) == 0 {
			return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("no selected targets match the mod"))
		}
		for _, id := range chosen {
			if err := checkAndPlan(id, filepath.ToSlash(filepath.Join(next.Pack.ModsDir, m.Filename)), m.SHA256); err != nil {
				return nil, err
			}
		}
		keep := m.Targets[:0]
		for _, id := range m.Targets {
			if !containsID(chosen, id) {
				keep = append(keep, id)
			}
		}
		if len(keep) == 0 {
			next.Mods = append(next.Mods[:idx], next.Mods[idx+1:]...)
		} else {
			next.Mods[idx].Targets = append([]string(nil), keep...)
		}
	case "file":
		out := next.Files[:0]
		for _, f := range next.Files {
			if f.Path != p.Identity {
				out = append(out, f)
				continue
			}
			found = true
			chosen := selected(f.Targets)
			if len(chosen) == 0 {
				out = append(out, f)
				continue
			}
			for _, id := range chosen {
				if err := checkAndPlan(id, f.Target, f.SHA256); err != nil {
					return nil, err
				}
			}
			keep := f.Targets[:0]
			for _, id := range f.Targets {
				if !containsID(chosen, id) {
					keep = append(keep, id)
				}
			}
			if len(keep) > 0 {
				f.Targets = append([]string(nil), keep...)
				out = append(out, f)
			}
		}
		next.Files = out
	}
	if p.Kind == "mod" {
		found = true
	}
	if !found {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("managed file %q was not found", p.Identity))
	}
	if err := next.Validate(); err != nil {
		return nil, failure.Wrap(failure.InvalidRequest, err)
	}
	for _, m := range mutations {
		if err := os.Remove(m.path); err != nil {
			restoreRemovals(mutations)
			return nil, err
		}
	}
	if err := lockfile.Write(lockPath, &next); err != nil {
		restoreRemovals(mutations)
		return nil, err
	}
	return map[string]any{"lock": &next, "removed_targets": removed}, nil
}

func restoreRemovals(items []removeMutation) {
	for _, m := range items {
		_ = atomicfile.Write(m.path, m.bytes, 0644)
	}
}
func containsID(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
