package sync

import (
	"fmt"
	"path/filepath"
	"strings"

	"modlock/internal/failure"
	"modlock/internal/lockfile"
)

// TargetRoots are trusted physical roots supplied by the launcher. They are
// request-scoped and are never read from or written to a lock file.
type TargetRoots map[string]string

// ValidateTargetRoots validates every supplied mapping and ensures all targets
// referenced by a schema 3 lock can be resolved. Legacy locks deliberately
// ignore the mapping and continue using the process root.
func ValidateTargetRoots(processRoot string, lock *lockfile.File, roots TargetRoots) error {
	if lock == nil || lock.Schema < 3 {
		return nil
	}
	used := map[string]string{}
	for _, mod := range lock.Mods {
		for _, id := range mod.Targets {
			used[id] = "mods/" + mod.Filename
		}
	}
	for _, file := range lock.Files {
		for _, id := range file.Targets {
			used[id] = file.Target
		}
	}
	canonical := map[string]string{}
	processAbs, err := filepath.Abs(filepath.Clean(processRoot))
	if err != nil {
		return failure.Wrap(failure.InvalidRequest, fmt.Errorf("invalid process root: %w", err))
	}
	for id, value := range roots {
		if err := lockfile.ValidateTargetID(id); err != nil {
			return failure.Wrap(failure.InvalidRequest, err)
		}
		if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) {
			return failure.Wrap(failure.InvalidRequest, fmt.Errorf("target root %q must be an absolute path", id))
		}
		abs, err := filepath.Abs(filepath.Clean(value))
		if err != nil {
			return failure.Wrap(failure.InvalidRequest, fmt.Errorf("invalid target root %q: %w", id, err))
		}
		if id == "client" && !strings.EqualFold(filepath.Clean(abs), filepath.Clean(processAbs)) {
			return failure.Wrap(failure.InvalidRequest, fmt.Errorf("client target root must match the bridge process root"))
		}
		key := strings.ToLower(filepath.Clean(abs))
		if previous, ok := canonical[key]; ok && previous != id {
			return failure.Wrap(failure.InvalidRequest, fmt.Errorf("target roots %q and %q resolve to the same directory", previous, id))
		}
		canonical[key] = id
		// ResolveWithin performs the same link/junction check used for managed paths.
		if _, err := lockfile.ResolveWithin(abs, ".modlock-target-root-check"); err != nil {
			return failure.Wrap(failure.InvalidRequest, fmt.Errorf("invalid target root %q: %w", id, err))
		}
	}
	for id, rel := range used {
		if roots[id] == "" {
			return failure.WrapDetails(failure.InvalidRequest, fmt.Errorf("launcher did not provide a root for target %q", id), map[string]any{"target": id, "path": rel})
		}
	}
	return nil
}

func targetRoot(processRoot string, schema int, roots TargetRoots, id string) (string, error) {
	if schema < 3 {
		return processRoot, nil
	}
	root := roots[id]
	if root == "" {
		return "", failure.WrapDetails(failure.InvalidRequest, fmt.Errorf("launcher did not provide a root for target %q", id), map[string]any{"target": id})
	}
	return filepath.Abs(filepath.Clean(root))
}

func resolveTargetPath(processRoot string, schema int, roots TargetRoots, id, rel string) (string, error) {
	root, err := targetRoot(processRoot, schema, roots, id)
	if err != nil {
		return "", err
	}
	return lockfile.ResolveWithin(root, rel)
}
