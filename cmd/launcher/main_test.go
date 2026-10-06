package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func init() {
	mode := os.Getenv("MODLOCK_TEST_HELPER")
	if mode == "" {
		return
	}
	if mode == "early-exit" {
		os.Exit(23)
	}
	if ready := os.Getenv("MODLOCK_READY_FILE"); ready != "" {
		_ = os.WriteFile(ready, []byte("ready"), 0600)
	}
	if record := os.Getenv("MODLOCK_TEST_RECORD"); record != "" {
		_ = json.NewEncoder(mustCreate(record)).Encode(struct {
			Args []string
			Dir  string
		}{os.Args[1:], mustGetwd()})
	}
	if mode == "ready-slow" {
		ms := 300
		if raw := os.Getenv("MODLOCK_TEST_DELAY_MS"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				ms = n
			}
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	if mode == "ready-error" {
		os.Exit(23)
	}
	os.Exit(0)
}

func mustCreate(path string) *os.File { f, _ := os.Create(path); return f }
func mustGetwd() string               { p, _ := os.Getwd(); return p }

func helperProcess(t *testing.T, mode string, args []string, record string, delay int) (int, int) {
	t.Helper()
	oldTimeout := startupTimeout
	startupTimeout = 100 * time.Millisecond
	t.Cleanup(func() { startupTimeout = oldTimeout })
	t.Setenv("MODLOCK_TEST_HELPER", mode)
	t.Setenv("MODLOCK_TEST_RECORD", record)
	t.Setenv("MODLOCK_TEST_DELAY_MS", strconv.Itoa(delay))
	return launchAndWait(os.Args[0], t.TempDir(), false, false, args)
}

func TestLaunchWaitsPastStartupTimeoutAfterReadinessAndForwardsArgs(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	args := []string{"argument with spaces", "кириллица"}
	started := time.Now()
	code, exitCode := helperProcess(t, "ready-slow", args, record, 350)
	if code != -1 || exitCode != 0 {
		t.Fatalf("launch result = %d/%d", code, exitCode)
	}
	if time.Since(started) < 300*time.Millisecond {
		t.Fatal("launcher returned before ready process completed")
	}
	var got struct {
		Args []string
		Dir  string
	}
	f, err := os.Open(record)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = json.NewDecoder(f).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Args, args) {
		t.Fatalf("forwarded args = %#v", got.Args)
	}
	wd, _ := os.Getwd()
	if got.Dir != wd {
		t.Fatalf("working directory = %q, want %q", got.Dir, wd)
	}
}

func TestLaunchReportsExitBeforeReadiness(t *testing.T) {
	code, exitCode := helperProcess(t, "early-exit", nil, "", 0)
	if code != -3 || exitCode != 23 {
		t.Fatalf("launch result = %d/%d, want startup failure/23", code, exitCode)
	}
}

func TestReadyProcessExitIsFinalAndKeepsItsExitCode(t *testing.T) {
	code, exitCode := helperProcess(t, "ready-error", nil, "", 0)
	if code != -1 || exitCode != 23 {
		t.Fatalf("launch result = %d/%d, want final exit 23", code, exitCode)
	}
}

func TestLaunchReportsMissingExecutable(t *testing.T) {
	code, exitCode := launchAndWait(filepath.Join(t.TempDir(), "missing.exe"), t.TempDir(), false, false, nil)
	if code != -3 || exitCode == 0 {
		t.Fatalf("launch result = %d/%d", code, exitCode)
	}
}

func TestPayloadLoopRecoversMissingEarlyExitAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first int
	}{{"missing active exe", 999}, {"exit before readiness", -3}, {"readiness timeout", -2}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			paths := map[string]string{}
			for _, version := range []string{"1.0.0", "2.0.0"} {
				paths[version] = filepath.Join(root, version, "app.exe")
			}
			if err := os.MkdirAll(filepath.Dir(paths["1.0.0"]), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths["1.0.0"], []byte("old"), 0755); err != nil {
				t.Fatal(err)
			}
			if tc.name != "missing active exe" {
				if err := os.MkdirAll(filepath.Dir(paths["2.0.0"]), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths["2.0.0"], []byte("new"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			active, previous, rejected := "2.0.0", "1.0.0", ""
			attempts := []string{}
			failedInitially := false
			stderr, err := os.Create(filepath.Join(root, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			d := launcherDeps{
				active: func() string { return active }, previous: func() string { return previous },
				appPath: func(v string) (string, error) {
					p, ok := paths[v]
					if !ok {
						return "", fmt.Errorf("bad version %q", v)
					}
					return p, nil
				},
				checkPayload: func(v string) error {
					if _, e := os.Stat(paths[v]); e != nil {
						return e
					}
					return nil
				},
				rollback: func(failed string) error {
					if active != failed || previous == "" {
						return fmt.Errorf("state changed")
					}
					rejected = failed
					active, previous = previous, failed
					return nil
				},
				bundled: func() string { return "" }, stderr: stderr,
				launch: func(path, _ string, _, _ bool, _ []string) (int, int) {
					attempts = append(attempts, path)
					if tc.name != "missing active exe" && !failedInitially {
						failedInitially = true
						return tc.first, 1
					}
					return -1, 0
				},
			}
			if code := runPayloadLoop(active, "", []string{"sync"}, root, d); code != 0 {
				t.Fatalf("runPayloadLoop = %d", code)
			}
			if active != "1.0.0" || previous != "2.0.0" || rejected != "2.0.0" {
				t.Fatalf("recovery state active=%q previous=%q rejected=%q", active, previous, rejected)
			}
			if tc.name == "missing active exe" {
				if len(attempts) != 1 || attempts[0] != paths["1.0.0"] {
					t.Fatalf("attempts = %#v", attempts)
				}
			} else if len(attempts) != 2 || attempts[0] != paths["2.0.0"] || attempts[1] != paths["1.0.0"] {
				t.Fatalf("attempts = %#v", attempts)
			}
			attempts = nil
			if code := runPayloadLoop(active, "", []string{"version"}, root, d); code != 0 {
				t.Fatalf("next launch = %d", code)
			}
			if len(attempts) != 1 || attempts[0] != paths["1.0.0"] {
				t.Fatalf("next launch did not select recovered version: %#v", attempts)
			}
		})
	}
}

func TestAutomaticUpdateFailureResumesOriginalCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure int
	}{{"early exit", -3}, {"timeout", -2}, {"missing payload", 999}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			paths := map[string]string{}
			for _, v := range []string{"1.0.0", "2.0.0"} {
				paths[v] = filepath.Join(root, v, "app.exe")
				if err := os.MkdirAll(filepath.Dir(paths[v]), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths[v], []byte(v), 0755); err != nil {
					t.Fatal(err)
				}
			}
			active, previous, rejected := "1.0.0", "", ""
			operations, rollbacks := 0, 0
			var launches []string
			args := []string{"sync"}
			stderr, err := os.Create(filepath.Join(root, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			d := launcherDeps{
				active: func() string { return active }, previous: func() string { return previous },
				appPath: func(v string) (string, error) {
					p, ok := paths[v]
					if !ok {
						return "", fmt.Errorf("unknown version %q", v)
					}
					return p, nil
				},
				checkPayload: func(v string) error { _, err := os.Stat(paths[v]); return err },
				rollback: func(failed string) error {
					if failed != active {
						return fmt.Errorf("stale pointer")
					}
					rollbacks++
					rejected = failed
					active, previous = previous, failed
					return nil
				},
				bundled: func() string { return "" }, stderr: stderr,
				launch: func(path, _ string, skipUpdate, handoff bool, gotArgs []string) (int, int) {
					launches = append(launches, path)
					if !reflect.DeepEqual(gotArgs, args) || handoff {
						t.Fatalf("incorrect launch args or handoff: %#v, %v", gotArgs, handoff)
					}
					if skipUpdate != (len(launches) > 1) {
						t.Fatalf("skipUpdate=%v on launch %d", skipUpdate, len(launches))
					}
					if len(launches) == 1 {
						active, previous = "2.0.0", "1.0.0"
						if tc.failure == 999 {
							if err := os.Remove(paths["2.0.0"]); err != nil {
								t.Fatal(err)
							}
						}
						return 75, -1
					}
					if path == paths["2.0.0"] {
						return tc.failure, 23
					}
					operations++
					return -1, 0
				},
			}
			if code := runPayloadLoop(active, "", args, root, d); code != 0 {
				t.Fatalf("command exit code = %d", code)
			}
			if active != "1.0.0" || previous != "2.0.0" || rejected != "2.0.0" || operations != 1 || rollbacks != 1 {
				t.Fatalf("active=%s previous=%s rejected=%s operations=%d rollbacks=%d", active, previous, rejected, operations, rollbacks)
			}
			want := []string{paths["1.0.0"], paths["2.0.0"], paths["1.0.0"]}
			if tc.failure == 999 {
				want = []string{paths["1.0.0"], paths["1.0.0"]}
			}
			if !reflect.DeepEqual(launches, want) {
				t.Fatalf("launches=%#v, want %#v", launches, want)
			}
		})
	}
}

func TestPayloadLoopBoundsRepeatedHandoffs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.exe")
	if err := os.WriteFile(path, []byte("payload"), 0755); err != nil {
		t.Fatal(err)
	}
	launches := 0
	d := launcherDeps{
		active:  func() string { return "1.0.0" },
		appPath: func(string) (string, error) { return path, nil },
		stderr:  os.Stderr,
		launch:  func(string, string, bool, bool, []string) (int, int) { launches++; return 75, -1 },
	}
	if code := runPayloadLoop("1.0.0", "", []string{"sync"}, root, d); code == 0 || launches != 5 {
		t.Fatalf("exit=%d launches=%d; expected bounded handoff failure", code, launches)
	}
}

func TestPayloadLoopDoesNotRepeatSelfUpdateAfterFailedHandoff(t *testing.T) {
	root := t.TempDir()
	paths := map[string]string{}
	for _, v := range []string{"1.0.0", "2.0.0"} {
		paths[v] = filepath.Join(root, v, "app.exe")
		_ = os.MkdirAll(filepath.Dir(paths[v]), 0755)
		_ = os.WriteFile(paths[v], []byte(v), 0755)
	}
	active, previous := "1.0.0", ""
	var launched []string
	var handoff []bool
	stderr, _ := os.Create(filepath.Join(root, "stderr"))
	defer stderr.Close()
	d := launcherDeps{
		active: func() string { return active }, previous: func() string { return previous },
		appPath: func(v string) (string, error) { return paths[v], nil }, checkPayload: func(string) error { return nil },
		rollback: func(failed string) error {
			if active != failed {
				return fmt.Errorf("stale active")
			}
			active, previous = previous, failed
			return nil
		},
		bundled: func() string { return "" }, stderr: stderr,
		launch: func(path, _ string, _, hand bool, _ []string) (int, int) {
			launched = append(launched, path)
			handoff = append(handoff, hand)
			if len(launched) == 1 {
				active, previous = "2.0.0", "1.0.0"
				return 75, -1
			}
			return -3, 23
		},
	}
	if code := runPayloadLoop(active, "", []string{"self-update"}, root, d); code == 0 {
		t.Fatal("failed new payload returned success")
	}
	if len(launched) != 2 || launched[0] != paths["1.0.0"] || launched[1] != paths["2.0.0"] {
		t.Fatalf("launches = %#v", launched)
	}
	if !handoff[1] {
		t.Fatalf("handoff flags = %#v", handoff)
	}
	if active != "1.0.0" || previous != "2.0.0" {
		t.Fatalf("pointers after failed update active=%q previous=%q", active, previous)
	}
}

func TestMissingUpdatedPayloadRestoresOldPointerAndFailsSelfUpdate(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "1.0.0", "app.exe")
	if err := os.MkdirAll(filepath.Dir(old), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	active, previous := "1.0.0", ""
	stderrPath := filepath.Join(root, "stderr")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	launches := 0
	d := launcherDeps{
		active: func() string { return active }, previous: func() string { return previous },
		appPath:      func(v string) (string, error) { return filepath.Join(root, v, "app.exe"), nil },
		checkPayload: func(string) error { return nil },
		rollback: func(failed string) error {
			if active != failed {
				return fmt.Errorf("stale pointer")
			}
			active, previous = previous, failed
			return nil
		},
		bundled: func() string { return "" }, stderr: stderr,
		launch: func(string, string, bool, bool, []string) (int, int) {
			launches++
			active, previous = "2.0.0", "1.0.0"
			return 75, -1
		},
	}
	if code := runPayloadLoop("1.0.0", "", []string{"self-update"}, root, d); code == 0 {
		t.Fatal("missing new payload reported update success")
	}
	if launches != 1 || active != "1.0.0" || previous != "2.0.0" {
		t.Fatalf("launches=%d active=%q previous=%q", launches, active, previous)
	}
	if _, err := stderr.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	msg, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(msg), "восстановлена предыдущая") {
		t.Fatalf("restoration message missing: %s", msg)
	}
}

func TestPayloadLoopAdoptsConcurrentPointerChange(t *testing.T) {
	root := t.TempDir()
	paths := map[string]string{}
	for _, version := range []string{"1.0.0", "1.5.0", "2.0.0"} {
		paths[version] = filepath.Join(root, version, "app.exe")
		_ = os.MkdirAll(filepath.Dir(paths[version]), 0755)
		_ = os.WriteFile(paths[version], []byte(version), 0755)
	}
	active, previous := "2.0.0", "1.0.0"
	var launches []string
	stderr, _ := os.Create(filepath.Join(root, "stderr"))
	defer stderr.Close()
	d := launcherDeps{
		active: func() string { return active }, previous: func() string { return previous },
		appPath: func(version string) (string, error) {
			if p, ok := paths[version]; ok {
				return p, nil
			}
			return "", fmt.Errorf("unknown version")
		},
		checkPayload: func(string) error { return nil },
		rollback: func(string) error {
			active, previous = "1.5.0", "2.0.0"
			return fmt.Errorf("active pointer changed concurrently")
		},
		bundled: func() string { return "" }, stderr: stderr,
		launch: func(path, _ string, _, _ bool, _ []string) (int, int) {
			launches = append(launches, path)
			if len(launches) == 1 {
				return -3, 1
			}
			return -1, 0
		},
	}
	if code := runPayloadLoop("2.0.0", "", []string{"sync"}, root, d); code != 0 {
		t.Fatalf("runPayloadLoop = %d", code)
	}
	if len(launches) != 2 || launches[0] != paths["2.0.0"] || launches[1] != paths["1.5.0"] {
		t.Fatalf("launches = %#v", launches)
	}
	if active != "1.5.0" || previous != "2.0.0" {
		t.Fatalf("concurrent pointers overwritten: active=%q previous=%q", active, previous)
	}
}

func TestPayloadLoopFallbackRunsSelfUpdateActionsNormally(t *testing.T) {
	for _, args := range [][]string{{"self-update"}, {"self-update", "--check"}, {"self-update", "--rollback"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			root := t.TempDir()
			paths := map[string]string{"1.0.0": filepath.Join(root, "active.exe"), "2.0.0": filepath.Join(root, "new.exe")}
			backup := filepath.Join(root, "backup.exe")
			for _, p := range []string{paths["1.0.0"], paths["2.0.0"], backup} {
				_ = os.WriteFile(p, []byte("payload"), 0755)
			}
			stderr, _ := os.Create(filepath.Join(root, "stderr"))
			defer stderr.Close()
			active, previous := "1.0.0", ""
			var launched []string
			var handoffs []bool
			operations := 0
			d := launcherDeps{active: func() string { return active }, previous: func() string { return previous }, appPath: func(v string) (string, error) {
				if p, ok := paths[v]; ok {
					return p, nil
				}
				return "", fmt.Errorf("bad version")
			}, checkPayload: func(string) error { return nil }, rollback: func(string) error { return fmt.Errorf("none") }, bundled: func() string { return backup }, stderr: stderr,
				launch: func(path, _ string, _, handoff bool, gotArgs []string) (int, int) {
					launched = append(launched, path)
					handoffs = append(handoffs, handoff)
					if len(launched) == 1 {
						return -3, 23
					}
					if path == backup {
						operations++
						if !reflect.DeepEqual(gotArgs, args) {
							t.Fatalf("fallback args = %#v, want %#v", gotArgs, args)
						}
						if len(args) > 1 && args[1] == "--check" {
							return -1, 0
						}
						active, previous = "2.0.0", "1.0.0"
						return 75, -1
					}
					if !handoff {
						t.Fatal("successful update/rollback handoff was not acknowledged")
					}
					return -1, 0
				}}
			if code := runPayloadLoop(active, "", args, root, d); code != 0 {
				t.Fatalf("runPayloadLoop = %d", code)
			}
			if operations != 1 {
				t.Fatalf("self-update action executed %d times", operations)
			}
			if len(args) > 1 && args[1] == "--check" {
				if len(launched) != 2 || handoffs[1] {
					t.Fatalf("check handoff/launches = %#v / %#v", handoffs, launched)
				}
			} else if len(launched) != 3 || handoffs[1] || !handoffs[2] {
				t.Fatalf("handoff/launches = %#v / %#v", handoffs, launched)
			}
		})
	}
}
