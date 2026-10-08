package bridge

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"modlock/internal/app"
	"modlock/internal/authorconfig"
	"modlock/internal/buildinfo"
	"modlock/internal/failure"
	"modlock/internal/lockfile"
	"modlock/internal/scan"
	syncer "modlock/internal/sync"
)

// Run deliberately bypasses the CLI menu and automatic payload updates.
func Run(args []string, input io.Reader, output io.Writer) error {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	protocol := fs.Int("protocol", 0, "protocol version")
	root := fs.String("root", "", "Minecraft instance directory")
	fail := func(err error) error { return json.NewEncoder(output).Encode(terminal("", nil, err)) }
	if err := fs.Parse(args); err != nil || len(fs.Args()) != 0 {
		return fail(failure.Wrap(failure.InvalidRequest, fmt.Errorf("usage: bridge --protocol 1 --root directory")))
	}
	if *protocol != Protocol {
		return fail(failure.Wrap(failure.UnsupportedProtocol, fmt.Errorf("supported bridge protocol: %d", Protocol)))
	}
	directory, err := app.ValidateRoot(*root)
	if err != nil {
		return fail(failure.Wrap(failure.InvalidRequest, err))
	}
	return Serve(context.Background(), input, output, func(ctx context.Context, request Request, progress func(string)) (any, error) {
		switch request.Operation {
		case "capabilities":
			return map[string]any{"protocol": Protocol, "version": buildinfo.Version, "loader_protocol": buildinfo.LoaderProtocol, "schemas": []int{1, 2, 3}, "operations": []string{"capabilities", "read", "install", "verify", "check", "apply", "scan", "save-author-settings", "author-state", "author-scan", "publish-preview", "publish", "add-mod", "remove-resource", "set-mod-targets", "set-resource-state", "set-tracked-path", "update-author-settings"}, "cancel": true}, nil
		case "read":
			var source lockfile.Pack
			if err := decode(request.Params, &source); err != nil {
				return nil, failure.Wrap(failure.InvalidRequest, err)
			}
			snapshot, err := syncer.Fetch(ctx, source, "", progress)
			if err != nil {
				return nil, err
			}
			defer snapshot.Close()
			return map[string]any{"revision": snapshot.Revision, "lock": snapshot.Lock}, nil
		case "install":
			var params struct {
				Pack               lockfile.Pack              `json:"pack"`
				Revision           string                     `json:"revision"`
				TargetRoots        syncer.TargetRoots         `json:"target_roots,omitempty"`
				ConfirmedConflicts []syncer.ConfirmedConflict `json:"confirmed_conflicts,omitempty"`
			}
			if err := decode(request.Params, &params); err != nil {
				return nil, failure.Wrap(failure.InvalidRequest, err)
			}
			if params.Revision == "" {
				return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("previewed revision is required"))
			}
			if existing, err := lockfile.FindPath(directory); err == nil {
				return nil, failure.Wrap(failure.Conflict, fmt.Errorf("instance already contains a ModLock manifest: %s", existing))
			} else if !os.IsNotExist(err) {
				return nil, err
			}
			snapshot, err := syncer.Fetch(ctx, params.Pack, params.Revision, progress)
			if err != nil {
				return nil, err
			}
			defer snapshot.Close()
			if err := syncer.ValidateInstallSchema(nil, snapshot.Lock); err != nil {
				return nil, err
			}
			if err := syncer.ValidateTargetRoots(directory, snapshot.Lock, params.TargetRoots); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// Seed the temporary instance with the exact previewed manifest. RunRevision
			// then performs the regular transactional install from that same commit.
			bootstrap := filepath.Join(directory, lockfile.DefaultFilename)
			bootstrapLock := &lockfile.File{Schema: snapshot.Lock.Schema, Pack: snapshot.Lock.Pack}
			if err := lockfile.Write(bootstrap, bootstrapLock); err != nil {
				return nil, err
			}
			result, err := syncer.RunRevisionConfirmedWithRoots(ctx, directory, params.Revision, progress, params.ConfirmedConflicts, params.TargetRoots)
			if err != nil {
				_ = os.Remove(bootstrap)
				return nil, err
			}
			return result, nil
		case "check":
			var params struct {
				TargetRoots syncer.TargetRoots `json:"target_roots,omitempty"`
			}
			if len(request.Params) != 0 && string(request.Params) != "null" {
				if err := decode(request.Params, &params); err != nil {
					return nil, failure.Wrap(failure.InvalidRequest, err)
				}
			}
			return syncer.CheckWithRoots(ctx, directory, params.TargetRoots, progress)
		case "verify":
			var params struct {
				TargetRoots syncer.TargetRoots `json:"target_roots,omitempty"`
			}
			if len(request.Params) != 0 && string(request.Params) != "null" {
				if err := decode(request.Params, &params); err != nil {
					return nil, failure.Wrap(failure.InvalidRequest, err)
				}
			}
			return syncer.VerifyWithRoots(directory, params.TargetRoots)
		case "promote-schema3-lock":
			var params struct {
				TargetRoots syncer.TargetRoots `json:"target_roots"`
			}
			if err := decode(request.Params, &params); err != nil {
				return nil, failure.Wrap(failure.InvalidRequest, err)
			}
			return promoteSchema3Lock(directory, params.TargetRoots)
		case "apply":
			var params struct {
				Revision           string                     `json:"revision"`
				TargetRoots        syncer.TargetRoots         `json:"target_roots,omitempty"`
				ConfirmedConflicts []syncer.ConfirmedConflict `json:"confirmed_conflicts,omitempty"`
			}
			if err := decode(request.Params, &params); err != nil {
				return nil, failure.Wrap(failure.InvalidRequest, err)
			}
			if params.Revision == "" {
				return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("previewed revision is required"))
			}
			return syncer.RunRevisionConfirmedWithRoots(ctx, directory, params.Revision, progress, params.ConfirmedConflicts, params.TargetRoots)
		case "save-author-settings":
			var settings authorconfig.Config
			if err := decode(request.Params, &settings); err != nil {
				return nil, failure.Wrap(failure.InvalidRequest, err)
			}
			if err := authorconfig.Write(directory, &settings); err != nil {
				return nil, err
			}
			return map[string]bool{"saved": true}, nil
		case "scan":
			var params struct {
				TargetRoots syncer.TargetRoots `json:"target_roots,omitempty"`
			}
			if len(request.Params) != 0 && string(request.Params) != "null" {
				if err := decode(request.Params, &params); err != nil {
					return nil, failure.Wrap(failure.InvalidRequest, err)
				}
			}
			if lockPath, lockErr := lockfile.FindPath(directory); lockErr == nil {
				manifest, readErr := lockfile.Read(lockPath)
				if readErr != nil {
					return nil, readErr
				}
				if manifest.Schema == 3 {
					if err := syncer.ValidateTargetRoots(directory, manifest, params.TargetRoots); err != nil {
						return nil, err
					}
					mods, err := scan.ModsForTargets(manifest, params.TargetRoots)
					if err != nil {
						return nil, err
					}
					return map[string]any{"mods": mods}, nil
				}
			} else if !os.IsNotExist(lockErr) {
				return nil, lockErr
			}
			mods, err := scan.Mods(directory)
			if err != nil {
				return nil, err
			}
			return map[string]any{"mods": mods}, nil
		case "author-state", "author-scan", "publish-preview", "publish", "add-mod", "remove-resource", "set-mod-targets", "set-resource-state", "set-tracked-path", "update-author-settings":
			return runAuthorOperation(ctx, directory, request, progress)
		default:
			return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("unsupported operation %q", request.Operation))
		}
	})
}
