package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"modlock/internal/authorconfig"
	"modlock/internal/failure"
	"modlock/internal/lockfile"
)

type fragmented struct{ io.Reader }

func (f fragmented) Read(b []byte) (int, error) {
	if len(b) > 2 {
		b = b[:2]
	}
	return f.Reader.Read(b)
}

func TestBridgeScansModsByContentHash(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mods", "пример.jar"), []byte("jar bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	input := `{"type":"request","id":"scan","operation":"scan"}`
	var output bytes.Buffer
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	got := events(t, output.Bytes())
	if len(got) != 1 || got[0].Error != nil {
		t.Fatalf("events: %#v", got)
	}
	var result struct {
		Mods []struct {
			Filename, SHA256 string
			Size             int64
		} `json:"mods"`
	}
	encoded, err := json.Marshal(got[0].Result)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Mods) != 1 || result.Mods[0].Filename != "пример.jar" || result.Mods[0].Size != int64(len("jar bytes")) || len(result.Mods[0].SHA256) != 64 {
		t.Fatalf("scan result: %#v", result)
	}
}

func TestBridgeSavesSeparateAuthorSettings(t *testing.T) {
	root := t.TempDir()
	input := `{"type":"request","id":"settings","operation":"save-author-settings","params":{"include_dirs":["config"],"exclude_paths":["saves/**"],"ignored_mod_ids":["modrinth:abc"]}}`
	var output bytes.Buffer
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	got := events(t, output.Bytes())
	if len(got) != 1 || got[0].Error != nil {
		t.Fatalf("events: %#v", got)
	}
	settings, err := authorconfig.Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.IncludeDirs) != 1 || len(settings.ExcludePaths) != 1 || len(settings.IgnoredModIDs) != 1 {
		t.Fatalf("settings: %#v", settings)
	}
}

func TestBridgeInstallsExactPreviewedSchema1Pack(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "pack source")
	if err := os.MkdirAll(filepath.Join(repository, "files", "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "files", "mods", "example.jar"), []byte("jar payload"), 0644); err != nil {
		t.Fatal(err)
	}
	pack := lockfile.Pack{Repository: repository, Branch: "master", LockPath: "mod.lock", ModsDir: "mods"}
	if err := lockfile.Write(filepath.Join(repository, "mod.lock"), &lockfile.File{Schema: 1, Pack: pack, Mods: []lockfile.ModEntry{{ID: "example", Filename: "example.jar", Source: "repo", Path: "files/mods/example.jar"}}}); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainInit(repository, false)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worktree.Add("."); err != nil {
		t.Fatal(err)
	}
	revision, err := worktree.Commit("publish pack", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "Новый экземпляр")
	if err = os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{"pack": pack, "revision": revision.String()})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]any{"type": "request", "id": "install", "operation": "install", "params": json.RawMessage(params)})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run([]string{"--protocol", "1", "--root", root}, bytes.NewReader(append(request, '\n')), &output); err != nil {
		t.Fatal(err)
	}
	events := events(t, output.Bytes())
	if len(events) < 2 || events[len(events)-1].Error != nil {
		t.Fatalf("install events: %#v", events)
	}
	if got, err := os.ReadFile(filepath.Join(root, "mods", "example.jar")); err != nil || string(got) != "jar payload" {
		t.Fatalf("installed jar = %q, %v", got, err)
	}
	installed, err := lockfile.Read(filepath.Join(root, lockfile.DefaultFilename))
	if err != nil || installed.Pack.Repository != repository {
		t.Fatalf("installed manifest = %#v, %v", installed, err)
	}
}

func events(t *testing.T, output []byte) []Event {
	t.Helper()
	var result []Event
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte{'\n'}) {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("non-JSON stdout: %q: %v", line, err)
		}
		if event.Protocol != Protocol {
			t.Fatalf("missing protocol: %q", line)
		}
		result = append(result, event)
	}
	return result
}

func TestFragmentedRequestProgressAndStructuredError(t *testing.T) {
	var output bytes.Buffer
	input := fragmented{strings.NewReader("{\"type\":\"request\",\"id\":\"a\",\"operation\":\"check\"}\n")}
	err := Serve(context.Background(), input, &output, func(_ context.Context, request Request, progress func(string)) (any, error) {
		if request.Operation != "check" {
			t.Errorf("wrong operation: %q", request.Operation)
		}
		progress("Получение\nсборки")
		return nil, failure.Wrap(failure.Network, errors.New("offline"))
	})
	if err != nil {
		t.Fatal(err)
	}
	got := events(t, output.Bytes())
	if len(got) != 2 || got[0].Type != "progress" || got[0].Message != "Получение\nсборки" || got[1].Type != "result" || got[1].Error.Code != failure.Network {
		t.Fatalf("events: %#v", got)
	}
}

func TestCancellationWaitsForRestoration(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	var output bytes.Buffer
	started, restored := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), reader, &output, func(ctx context.Context, _ Request, _ func(string)) (any, error) {
			close(started)
			<-ctx.Done()
			// Model a handler that must restore its transaction before returning.
			close(restored)
			return nil, ctx.Err()
		})
	}()
	if _, err := fmt.Fprintln(writer, `{"type":"request","id":"a","operation":"apply"}`); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := fmt.Fprintln(writer, `{"type":"cancel","id":"a"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	select {
	case <-restored:
	default:
		t.Fatal("process ended before restoration")
	}
	got := events(t, output.Bytes())
	if len(got) != 1 || got[0].Error == nil || got[0].Error.Code != failure.Cancelled {
		t.Fatalf("events: %#v", got)
	}
}

func TestProtocolCompatibilityAndCapabilities(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		var output bytes.Buffer
		err := Run([]string{"--protocol", version, "--root", t.TempDir()}, strings.NewReader(`{"type":"request","id":"a","operation":"capabilities"}`), &output)
		if err != nil {
			t.Fatal(err)
		}
		got := events(t, output.Bytes())
		if len(got) != 1 || got[0].Type != "result" {
			t.Fatalf("events: %#v", got)
		}
		if version == "1" && (got[0].Error != nil || got[0].Result == nil) {
			t.Fatalf("capabilities: %#v", got)
		}
		if version == "2" && (got[0].Error == nil || got[0].Error.Code != failure.UnsupportedProtocol) {
			t.Fatalf("compatibility: %#v", got)
		}
	}
}

func TestMalformedAndOversizedMessages(t *testing.T) {
	for _, input := range []string{"{broken}\n", `{"type":"request","id":"a","operation":"check","unknown":true}`, strings.Repeat("x", MaxMessageBytes+1)} {
		var output bytes.Buffer
		called := false
		err := Serve(context.Background(), strings.NewReader(input), &output, func(context.Context, Request, func(string)) (any, error) { called = true; return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		got := events(t, output.Bytes())
		if called || len(got) != 1 || got[0].Error == nil || got[0].Error.Code != failure.InvalidRequest {
			t.Fatalf("events: %#v, called=%v", got, called)
		}
	}
}
