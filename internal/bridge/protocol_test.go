package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

func TestBridgeAdvertisesAndRunsOfflineVerify(t *testing.T) {
	root := t.TempDir()
	mods := filepath.Join(root, "mods")
	if err := os.MkdirAll(mods, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mods, "local.jar"), []byte("local bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	pack := lockfile.Pack{Repository: "https://offline.invalid/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods"}
	manifest := &lockfile.File{Schema: 1, Pack: pack, Mods: []lockfile.ModEntry{{Filename: "local.jar", Source: "repo", Path: "files/local.jar"}}}
	if err := lockfile.Write(filepath.Join(root, lockfile.DefaultFilename), manifest); err != nil {
		t.Fatal(err)
	}

	var capabilities bytes.Buffer
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(`{"type":"request","id":"caps","operation":"capabilities"}`), &capabilities); err != nil {
		t.Fatal(err)
	}
	capEvents := events(t, capabilities.Bytes())
	encoded, err := json.Marshal(capEvents[0].Result)
	if err != nil {
		t.Fatal(err)
	}
	var capResult struct {
		Operations []string `json:"operations"`
	}
	if err = json.Unmarshal(encoded, &capResult); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(capResult.Operations, "verify") {
		t.Fatalf("operations omit verify: %#v", capResult.Operations)
	}

	var output bytes.Buffer
	request := `{"type":"request","id":"verify","operation":"verify"}`
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(request), &output); err != nil {
		t.Fatal(err)
	}
	verifyEvents := events(t, output.Bytes())
	encoded, err = json.Marshal(verifyEvents[0].Result)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		State         string   `json:"state"`
		NeedsRecovery bool     `json:"needs_recovery"`
		Unverified    []string `json:"unverified"`
	}
	if err = json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "unverified" || result.NeedsRecovery || len(result.Unverified) != 1 || result.Unverified[0] != "local.jar" {
		t.Fatalf("offline verify result: %#v", result)
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

func TestAuthorStateAndTargetMutationUseTypedBridgeData(t *testing.T) {
	root := filepath.Join(t.TempDir(), "minecraft")
	server := filepath.Join(t.TempDir(), "server")
	if err := os.MkdirAll(filepath.Join(root, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(server, "mods"), 0755); err != nil {
		t.Fatal(err)
	}
	clientData, serverData := []byte("same jar"), []byte("wrong jar")
	if err := os.WriteFile(filepath.Join(root, "mods", "same.jar"), clientData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server, "mods", "same.jar"), serverData, 0644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(clientData)
	pack := lockfile.Pack{Repository: "https://example.test/pack.git", Branch: "main", LockPath: "mod.lock", ModsDir: "mods", Name: "Pack", Version: "1"}
	lock := &lockfile.File{Schema: 3, Pack: pack, Mods: []lockfile.ModEntry{{ID: "modrinth:shared", Filename: "same.jar", Version: "1.2", Source: "repo", Path: "files/mods/same.jar", SHA256: hex.EncodeToString(h[:]), Targets: []string{"server", "client"}}}}
	if err := lockfile.Write(filepath.Join(root, "mod.lock"), lock); err != nil {
		t.Fatal(err)
	}
	settings := &authorconfig.Config{TrackedPaths: []authorconfig.TrackedPath{{Path: "kubejs", Targets: []string{"client", "server"}, Policy: "replace"}}}
	if err := authorconfig.Write(root, settings); err != nil {
		t.Fatal(err)
	}
	for _, targetRoot := range []string{root, server} {
		if err := os.MkdirAll(filepath.Join(targetRoot, "kubejs"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(targetRoot, "kubejs", "startup.js"), []byte("StartupEvents.registry('item', event => {})\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	input := `{"type":"request","id":"state","operation":"author-state","params":{"target_roots":{"client":` + quote(root) + `,"server":` + quote(server) + `}}}`
	var output bytes.Buffer
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	stateEvents := events(t, output.Bytes())
	if len(stateEvents) != 1 || stateEvents[0].Error != nil {
		t.Fatalf("author-state failed: %#v", stateEvents)
	}
	encoded, err := json.Marshal(stateEvents[0].Result)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Mods []struct {
			Identity     string              `json:"identity"`
			Targets      []string            `json:"targets"`
			Status       string              `json:"status"`
			TargetStates []AuthorTargetState `json:"target_states"`
		} `json:"mods"`
		Tracked []TrackedPathState `json:"tracked_paths"`
	}
	if err = json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Mods) != 1 || result.Mods[0].Identity != "modrinth:shared" || result.Mods[0].Status != "modified" || len(result.Mods[0].Targets) != 2 || len(result.Mods[0].TargetStates) != 2 || len(result.Tracked) != 1 {
		t.Fatalf("unexpected author state: %s", encoded)
	}
	mutation := `{"type":"request","id":"targets","operation":"set-mod-targets","params":{"id":"modrinth:shared","targets":["client"]}}`
	output.Reset()
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(mutation), &output); err != nil {
		t.Fatal(err)
	}
	got := events(t, output.Bytes())
	if len(got) != 1 || got[0].Error != nil {
		t.Fatalf("target mutation failed: %#v", got)
	}
	updated, err := lockfile.Read(filepath.Join(root, "mod.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Mods[0].Targets) != 1 || updated.Mods[0].Targets[0] != "client" {
		t.Fatalf("target mutation not persisted: %#v", updated.Mods[0].Targets)
	}

	rootsParams := `"target_roots":{"client":` + quote(root) + `,"server":` + quote(server) + `}`
	output.Reset()
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(`{"type":"request","id":"scan","operation":"author-scan","params":{`+rootsParams+`}}`), &output); err != nil {
		t.Fatal(err)
	}
	scanEvents := events(t, output.Bytes())
	if len(scanEvents) != 1 || scanEvents[0].Error != nil {
		t.Fatalf("author-scan failed: %#v", scanEvents)
	}
	scanJSON, _ := json.Marshal(scanEvents[0].Result)
	var scanObject map[string]json.RawMessage
	if err := json.Unmarshal(scanJSON, &scanObject); err != nil {
		t.Fatal(err)
	}
	if _, ok := scanObject["preview_id"]; ok {
		t.Fatalf("author-scan must return state only: %s", scanJSON)
	}

	output.Reset()
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(`{"type":"request","id":"preview","operation":"publish-preview","params":{`+rootsParams+`}}`), &output); err != nil {
		t.Fatal(err)
	}
	previewEvents := events(t, output.Bytes())
	if len(previewEvents) != 1 || previewEvents[0].Error != nil {
		t.Fatalf("publish-preview failed: %#v", previewEvents)
	}
	previewJSON, _ := json.Marshal(previewEvents[0].Result)
	var preview AuthorScanResult
	if err := json.Unmarshal(previewJSON, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.PreviewID == "" || preview.PlannedLock == nil || len(preview.Files.Added) != 2 || len(preview.PlannedLock.Files) != 1 || len(preview.PlannedLock.Files[0].Targets) != 2 {
		t.Fatalf("unexpected publish preview: %s", previewJSON)
	}
	if preview.TargetChanges == nil {
		t.Fatalf("target_changes must be an empty or populated list: %s", previewJSON)
	}
	if err := os.WriteFile(filepath.Join(root, "kubejs", "startup.js"), []byte("changed after preview\n"), 0644); err != nil {
		t.Fatal(err)
	}
	stale := `{"type":"request","id":"publish","operation":"publish","params":{"preview_id":` + quote(preview.PreviewID) + `,"target_roots":{"client":` + quote(root) + `,"server":` + quote(server) + `}}}`
	output.Reset()
	if err := Run([]string{"--protocol", "1", "--root", root}, strings.NewReader(stale), &output); err != nil {
		t.Fatal(err)
	}
	staleEvents := events(t, output.Bytes())
	if len(staleEvents) != 1 || staleEvents[0].Error == nil || staleEvents[0].Error.Details["kind"] != "changed_during_apply" {
		t.Fatalf("stale publish was not rejected with a stable conflict kind: %#v", staleEvents)
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

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
