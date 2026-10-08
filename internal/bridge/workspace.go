package bridge

import (
	"fmt"
	"os"
	"path/filepath"

	"modlock/internal/failure"
	"modlock/internal/lockfile"
	syncer "modlock/internal/sync"
)

// promoteSchema3Lock moves a legacy client-root lock into the instance
// workspace while retaining the exact previous bytes in a sibling backup.
// This is an explicit launcher action; it is never performed during import,
// check, or update.
func promoteSchema3Lock(workspace string, roots syncer.TargetRoots) (any, error) {
	if _, err := lockfile.FindPath(workspace); err == nil {
		return nil, failure.Wrap(failure.Conflict, fmt.Errorf("the workspace already contains a ModLock manifest"))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	client := roots["client"]
	if client == "" {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("launcher did not provide the client target root"))
	}
	oldPath, err := lockfile.FindPath(client)
	if err != nil {
		return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("no client-root ModLock manifest was found: %w", err))
	}
	old, err := lockfile.Read(oldPath)
	if err != nil {
		return nil, err
	}
	if old.Schema != 3 {
		return nil, failure.Wrap(failure.UnsupportedFormat, fmt.Errorf("only schema 3 locks can be promoted into a workspace"))
	}
	if err := syncer.ValidateTargetRoots(workspace, old, roots); err != nil {
		return nil, err
	}
	backup := oldPath + ".workspace.bak"
	if _, err := os.Lstat(backup); err == nil {
		return nil, failure.Wrap(failure.Conflict, fmt.Errorf("workspace backup already exists: %s", backup))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	newPath := filepath.Join(workspace, lockfile.DefaultFilename)
	if err := lockfile.Write(newPath, old); err != nil {
		return nil, err
	}
	if err := os.Rename(oldPath, backup); err != nil {
		_ = os.Remove(newPath)
		return nil, err
	}
	return map[string]any{"path": newPath, "backup": backup, "schema": old.Schema}, nil
}
