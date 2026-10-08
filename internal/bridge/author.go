package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"modlock/internal/authorconfig"
	"modlock/internal/failure"
	"modlock/internal/lockfile"
	"modlock/internal/scan"
	syncer "modlock/internal/sync"
)

type AuthorTargetState struct {
	TargetID string `json:"target_id"`
	Status   string `json:"status"`
	SHA256   string `json:"sha256,omitempty"`
	Message  string `json:"message,omitempty"`
}

type AuthorMod struct {
	ID           string              `json:"id"`
	Identity     string              `json:"identity"`
	Name         string              `json:"name"`
	IconURL      string              `json:"icon_url,omitempty"`
	Provider     string              `json:"provider"`
	ProjectID    string              `json:"project_id,omitempty"`
	Version      string              `json:"version"`
	VersionID    string              `json:"version_id,omitempty"`
	Filename     string              `json:"filename"`
	Source       string              `json:"source"`
	SHA256       string              `json:"sha256,omitempty"`
	Targets      []string            `json:"targets"`
	Managed      bool                `json:"managed"`
	Status       string              `json:"status"`
	TargetStates []AuthorTargetState `json:"target_states,omitempty"`
}

type AuthorFile struct {
	Path         string              `json:"path"`
	Target       string              `json:"target"`
	SHA256       string              `json:"sha256"`
	Policy       string              `json:"policy"`
	Targets      []string            `json:"targets"`
	Managed      bool                `json:"managed"`
	Status       string              `json:"status"`
	TargetStates []AuthorTargetState `json:"target_states,omitempty"`
}

type TrackedPathState struct {
	Path     string   `json:"path"`
	Targets  []string `json:"targets"`
	Policy   string   `json:"policy"`
	Enabled  bool     `json:"enabled"`
	Excluded bool     `json:"excluded"`
	Status   string   `json:"status"`
}

type AuthorState struct {
	Lock         *lockfile.File       `json:"lock"`
	Settings     *authorconfig.Config `json:"settings"`
	Mods         []AuthorMod          `json:"mods"`
	Files        []AuthorFile         `json:"files"`
	TrackedPaths []TrackedPathState   `json:"tracked_paths"`
	Branch       string               `json:"branch"`
	Dirty        bool                 `json:"dirty"`
}

type authorRootsRequest struct {
	TargetRoots syncer.TargetRoots `json:"target_roots,omitempty"`
}

func runAuthorOperation(ctx context.Context, root string, request Request, progress func(string)) (any, error) {
	switch request.Operation {
	case "author-state":
		var params authorRootsRequest
		if err := decodeOptional(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		state, err := readAuthorState(root, params.TargetRoots)
		if err != nil {
			return nil, err
		}
		return state, nil
	case "author-scan":
		var params authorRootsRequest
		if err := decodeOptional(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		return readAuthorState(root, params.TargetRoots)
	case "publish-preview":
		var params authorRootsRequest
		if err := decodeOptional(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		return buildAuthorScan(root, params.TargetRoots)
	case "publish":
		var params struct {
			TargetRoots   syncer.TargetRoots `json:"target_roots"`
			PreviewID     string             `json:"preview_id"`
			CommitMessage string             `json:"commit_message,omitempty"`
		}
		if err := decode(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		return publishAuthor(ctx, root, params.TargetRoots, params.PreviewID, params.CommitMessage)
	case "add-mod":
		var params addModRequest
		if err := decode(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		return addAuthorMod(root, params)
	case "remove-resource":
		var params removeResourceRequest
		if err := decode(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		return removeAuthorResource(root, params)
	case "set-mod-targets":
		var params struct {
			ID      string   `json:"id"`
			Targets []string `json:"targets"`
		}
		if err := decode(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		path, lock, err := authorLock(root)
		if err != nil {
			return nil, err
		}
		if lock.Schema != 3 {
			return nil, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("set-mod-targets requires schema 3"))
		}
		found := false
		for i := range lock.Mods {
			if lock.Mods[i].Identity() == params.ID || lock.Mods[i].ID == params.ID {
				lock.Mods[i].Targets = append([]string(nil), params.Targets...)
				found = true
				break
			}
		}
		if !found {
			return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("mod %q is not managed", params.ID))
		}
		if err := lock.Validate(); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		if err := lockfile.Write(path, lock); err != nil {
			return nil, err
		}
		return map[string]any{"lock": lock}, nil
	case "set-resource-state":
		var params struct {
			Kind     string `json:"kind"`
			Identity string `json:"identity"`
			State    string `json:"state"`
		}
		if err := decode(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		path, lock, err := authorLock(root)
		if err != nil {
			return nil, err
		}
		settings, err := authorconfig.Read(root)
		if err != nil {
			return nil, err
		}
		if lock.Schema == 3 {
			switch params.Kind {
			case "mod":
				idx := -1
				identity := params.Identity
				for i, m := range lock.Mods {
					if m.Identity() == identity || m.ID == identity {
						idx = i
						identity = m.Identity()
						break
					}
				}
				if params.State == "managed" {
					if idx < 0 {
						return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("cannot manage unknown mod without provider data"))
					}
					removeString(&settings.IgnoredModIDs, identity)
				} else if params.State == "unmanaged" || params.State == "ignored" {
					if idx >= 0 {
						lock.Mods = append(lock.Mods[:idx], lock.Mods[idx+1:]...)
					}
					if params.State == "ignored" {
						appendUnique(&settings.IgnoredModIDs, identity)
					} else {
						removeString(&settings.IgnoredModIDs, identity)
					}
				} else {
					return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("state must be managed, unmanaged, or ignored"))
				}
			case "file":
				idx := -1
				target := ""
				for i, f := range lock.Files {
					if f.Path == params.Identity {
						idx = i
						target = f.Target
						break
					}
				}
				if params.State == "managed" {
					if idx < 0 {
						return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("cannot manage unknown file without tracked path settings"))
					}
				} else if params.State == "unmanaged" || params.State == "ignored" {
					if idx >= 0 {
						lock.Files = append(lock.Files[:idx], lock.Files[idx+1:]...)
					}
					if params.State == "ignored" && target != "" {
						appendUnique(&settings.ExcludePaths, target)
					}
				} else {
					return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("state must be managed, unmanaged, or ignored"))
				}
			default:
				return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("kind must be mod or file"))
			}
			if err := lock.Validate(); err != nil {
				return nil, failure.Wrap(failure.InvalidRequest, err)
			}
			if err := lockfile.Write(path, lock); err != nil {
				return nil, err
			}
		}
		if err := authorconfig.Write(root, settings); err != nil {
			return nil, err
		}
		return map[string]any{"lock": lock, "settings": settings}, nil
	case "set-tracked-path":
		var params struct {
			Path    string   `json:"path"`
			Targets []string `json:"targets"`
			Policy  string   `json:"policy"`
			Enabled bool     `json:"enabled"`
		}
		if err := decode(request.Params, &params); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		settings, err := authorconfig.Read(root)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(params.Path))))
		idx := -1
		for i, p := range settings.TrackedPaths {
			if strings.ToLower(filepath.ToSlash(filepath.Clean(filepath.FromSlash(p.Path)))) == key {
				idx = i
				break
			}
		}
		if params.Enabled {
			entry := authorconfig.TrackedPath{Path: params.Path, Targets: append([]string(nil), params.Targets...), Policy: params.Policy}
			if idx < 0 {
				settings.TrackedPaths = append(settings.TrackedPaths, entry)
			} else {
				settings.TrackedPaths[idx] = entry
			}
		} else if idx >= 0 {
			settings.TrackedPaths = append(settings.TrackedPaths[:idx], settings.TrackedPaths[idx+1:]...)
		}
		if err := settings.Validate(); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		if err := authorconfig.Write(root, settings); err != nil {
			return nil, err
		}
		return map[string]any{"settings": settings}, nil
	case "update-author-settings":
		var settings authorconfig.Config
		if err := decode(request.Params, &settings); err != nil {
			return nil, failure.Wrap(failure.InvalidRequest, err)
		}
		if err := authorconfig.Write(root, &settings); err != nil {
			return nil, err
		}
		return map[string]any{"settings": settings}, nil
	default:
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("unsupported author operation %q", request.Operation))
	}
}

func readAuthorState(root string, roots syncer.TargetRoots) (*AuthorState, error) {
	path, lock, err := authorLock(root)
	if err != nil {
		return nil, err
	}
	settings, err := authorconfig.Read(root)
	if err != nil {
		return nil, err
	}
	if lock.Schema == 3 {
		if err := syncer.ValidateTargetRoots(root, lock, roots); err != nil {
			return nil, err
		}
		var needed []string
		for _, p := range settings.TrackedPaths {
			needed = append(needed, p.Targets...)
		}
		for range settings.IncludeDirs {
			needed = append(needed, "client")
		}
		probe := &lockfile.File{Schema: 3}
		for _, id := range needed {
			probe.Mods = append(probe.Mods, lockfile.ModEntry{Targets: []string{id}})
		}
		if err := syncer.ValidateTargetRoots(root, probe, roots); err != nil {
			return nil, err
		}
	}
	state := &AuthorState{Lock: lock, Settings: settings, Branch: lock.Pack.Branch, Mods: []AuthorMod{}, Files: []AuthorFile{}, TrackedPaths: []TrackedPathState{}}
	modsByTarget := map[string]map[string]scan.Mod{}
	if lock.Schema == 3 {
		mods, err := scan.ModsForTargets(lock, roots)
		if err != nil {
			return nil, err
		}
		for _, m := range mods {
			if modsByTarget[m.TargetID] == nil {
				modsByTarget[m.TargetID] = map[string]scan.Mod{}
			}
			modsByTarget[m.TargetID][strings.ToLower(m.Filename)] = m
		}
	} else {
		mods, err := scan.Mods(root)
		if err != nil {
			return nil, err
		}
		modsByTarget["client"] = map[string]scan.Mod{}
		for _, m := range mods {
			modsByTarget["client"][strings.ToLower(m.Filename)] = m
		}
	}
	managedKeys := map[string]bool{}
	for _, mod := range lock.Mods {
		ids := mod.Targets
		if lock.Schema < 3 {
			ids = []string{"client"}
		}
		name := mod.Name
		if name == "" {
			name = mod.ID
		}
		item := AuthorMod{ID: mod.ID, Identity: mod.Identity(), Name: name, IconURL: mod.IconURL, Provider: mod.Source, ProjectID: mod.ProjectID, Version: mod.DisplayVersion(), VersionID: mod.VersionID, Filename: mod.Filename, Source: mod.Source, SHA256: mod.SHA256, Targets: append([]string(nil), ids...), Managed: true, Status: "synced"}
		if item.Name == "" {
			item.Name = mod.Filename
		}
		for _, id := range ids {
			managedKeys[authorResourceKey(id, mod.Filename)] = true
			st := readTargetStatus(root, lock.Schema, roots, id, filepath.ToSlash(filepath.Join(lock.Pack.ModsDir, mod.Filename)), mod.SHA256)
			item.TargetStates = append(item.TargetStates, AuthorTargetState{TargetID: id, Status: st.status, SHA256: st.sha256, Message: st.message})
		}
		item.Status = aggregateStatus(item.TargetStates)
		state.Mods = append(state.Mods, item)
	}
	for id, items := range modsByTarget {
		for _, m := range items {
			if managedKeys[authorResourceKey(id, m.Filename)] {
				continue
			}
			status := "unmanaged"
			if settings.ContainsIgnored(m.ID) {
				status = "ignored"
			}
			state.Mods = append(state.Mods, AuthorMod{ID: m.ID, Identity: m.ID, Name: m.Filename, Filename: m.Filename, Source: m.Source, Provider: m.Source, SHA256: m.SHA256, Targets: []string{}, Managed: false, Status: status, TargetStates: []AuthorTargetState{{TargetID: id, Status: status, SHA256: m.SHA256}}})
		}
	}
	for _, f := range lock.Files {
		ids := f.Targets
		if lock.Schema < 3 {
			ids = []string{"client"}
		}
		item := AuthorFile{Path: f.Path, Target: f.Target, SHA256: f.SHA256, Policy: f.Policy, Targets: append([]string(nil), ids...), Managed: true, Status: "synced"}
		for _, id := range ids {
			st := readTargetStatus(root, lock.Schema, roots, id, f.Target, f.SHA256)
			item.TargetStates = append(item.TargetStates, AuthorTargetState{TargetID: id, Status: st.status, SHA256: st.sha256, Message: st.message})
		}
		item.Status = aggregateStatus(item.TargetStates)
		state.Files = append(state.Files, item)
	}
	for _, p := range settings.TrackedPaths {
		status := trackedPathStatus(state.Files, p.Path, p.Targets, root, lock.Schema, roots)
		state.TrackedPaths = append(state.TrackedPaths, TrackedPathState{Path: p.Path, Targets: append([]string(nil), p.Targets...), Policy: p.Policy, Enabled: true, Status: status})
	}
	for _, p := range settings.IncludeDirs {
		state.TrackedPaths = append(state.TrackedPaths, TrackedPathState{Path: p, Targets: []string{"client"}, Policy: "replace", Enabled: true, Status: trackedPathStatus(state.Files, p, []string{"client"}, root, lock.Schema, roots)})
	}
	for _, p := range settings.ExcludePaths {
		state.TrackedPaths = append(state.TrackedPaths, TrackedPathState{Path: p, Enabled: false, Excluded: true, Status: "ignored"})
	}
	sort.Slice(state.Mods, func(i, j int) bool {
		return state.Mods[i].Identity+state.Mods[i].Filename < state.Mods[j].Identity+state.Mods[j].Filename
	})
	sort.Slice(state.Files, func(i, j int) bool { return state.Files[i].Path < state.Files[j].Path })
	_ = path
	return state, nil
}

func trackedPathStatus(files []AuthorFile, tracked string, targetIDs []string, root string, schema int, roots syncer.TargetRoots) string {
	base := filepath.ToSlash(filepath.Clean(filepath.FromSlash(tracked)))
	var states []AuthorTargetState
	for _, file := range files {
		rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Target)))
		if base != "." && rel != base && !strings.HasPrefix(rel, strings.TrimSuffix(base, "/")+"/") {
			continue
		}
		for _, state := range file.TargetStates {
			for _, id := range targetIDs {
				if state.TargetID == id {
					states = append(states, state)
				}
			}
		}
	}
	if len(states) > 0 {
		return aggregateStatus(states)
	}
	for _, id := range targetIDs {
		targetRoot := root
		if schema == 3 {
			targetRoot = roots[id]
		}
		if targetRoot == "" {
			return "conflict"
		}
		path, err := lockfile.ResolveWithin(targetRoot, tracked)
		if err != nil {
			return "conflict"
		}
		if _, err = os.Lstat(path); os.IsNotExist(err) {
			return "missing"
		} else if err != nil {
			return "conflict"
		}
	}
	return "unmanaged"
}

type targetStatus struct{ status, sha256, message string }

func readTargetStatus(root string, schema int, roots syncer.TargetRoots, id, rel, expected string) targetStatus {
	targetRoot := root
	if schema == 3 {
		targetRoot = roots[id]
	}
	if targetRoot == "" {
		return targetStatus{status: "conflict", message: "launcher root is missing"}
	}
	path, err := lockfile.ResolveWithin(targetRoot, rel)
	if err != nil {
		return targetStatus{status: "conflict", message: err.Error()}
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return targetStatus{status: "missing"}
	}
	if err != nil {
		return targetStatus{status: "conflict", message: err.Error()}
	}
	if !info.Mode().IsRegular() {
		return targetStatus{status: "conflict", message: "target is not a regular file"}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return targetStatus{status: "conflict", message: err.Error()}
	}
	h := sha256.Sum256(b)
	got := hex.EncodeToString(h[:])
	if expected != "" && !strings.EqualFold(got, expected) {
		return targetStatus{status: "modified", sha256: got}
	}
	return targetStatus{status: "synced", sha256: got}
}
func aggregateStatus(states []AuthorTargetState) string {
	result := "synced"
	rank := map[string]int{"synced": 0, "unmanaged": 1, "modified": 2, "missing": 3, "ignored": 4, "conflict": 5}
	for _, s := range states {
		if rank[s.Status] > rank[result] {
			result = s.Status
		}
	}
	return result
}
func authorLock(root string) (string, *lockfile.File, error) {
	p, err := lockfile.FindPath(root)
	if err != nil {
		return "", nil, err
	}
	f, err := lockfile.Read(p)
	return p, f, err
}
func decodeOptional(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return decode(raw, target)
}
func appendUnique(values *[]string, value string) {
	for _, v := range *values {
		if v == value {
			return
		}
	}
	*values = append(*values, value)
}
func removeString(values *[]string, value string) {
	out := (*values)[:0]
	for _, v := range *values {
		if v != value {
			out = append(out, v)
		}
	}
	*values = out
}

func authorResourceKey(targetID, name string) string {
	return strings.ToLower(targetID + "\x00" + name)
}

var _ = context.Canceled
