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
			return map[string]any{"protocol": Protocol, "version": buildinfo.Version, "loader_protocol": buildinfo.LoaderProtocol, "schemas": []int{1}, "operations": []string{"capabilities", "read", "install", "check", "apply", "scan", "save-author-settings"}, "cancel": true}, nil
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
				Pack     lockfile.Pack `json:"pack"`
				Revision string        `json:"revision"`
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
			// Seed the temporary instance with the exact previewed manifest. RunRevision
			// then performs the regular transactional install from that same commit.
			bootstrap := filepath.Join(directory, lockfile.DefaultFilename)
			if err := lockfile.Write(bootstrap, snapshot.Lock); err != nil {
				return nil, err
			}
			return syncer.RunRevision(ctx, directory, params.Revision, progress)
		case "check":
			return syncer.Check(ctx, directory, progress)
		case "apply":
			var params struct {
				Revision string `json:"revision"`
			}
			if err := decode(request.Params, &params); err != nil {
				return nil, failure.Wrap(failure.InvalidRequest, err)
			}
			if params.Revision == "" {
				return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("previewed revision is required"))
			}
			return syncer.RunRevision(ctx, directory, params.Revision, progress)
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
			mods, err := scan.Mods(directory)
			if err != nil {
				return nil, err
			}
			return map[string]any{"mods": mods}, nil
		default:
			return nil, failure.Wrap(failure.InvalidRequest, fmt.Errorf("unsupported operation %q", request.Operation))
		}
	})
}
