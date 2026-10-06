// modlock.exe is intentionally small and stable. It owns process handoff and
// never replaces its own executable while Windows has it open.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"modlock/internal/buildinfo"
	"modlock/internal/updater"
)

func main() { os.Exit(run()) }

func run() int {
	data, err := updater.DataDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ModLock launcher:", err)
		return 1
	}
	controlDir := data
	if err = os.MkdirAll(data, 0755); err != nil {
		fmt.Fprintln(os.Stderr, "ModLock: update storage is unavailable; using the bundled app:", err)
		controlDir, err = os.MkdirTemp("", "modlock-launcher-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "ModLock launcher:", err)
			return 1
		}
		defer os.RemoveAll(controlDir)
	} else if probe, probeErr := os.CreateTemp(data, ".write-check-"); probeErr != nil {
		fmt.Fprintln(os.Stderr, "ModLock: update storage is unavailable; using the bundled app:", probeErr)
		controlDir, err = os.MkdirTemp("", "modlock-launcher-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "ModLock launcher:", err)
			return 1
		}
		defer os.RemoveAll(controlDir)
	} else {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
	}
	versions, err := updater.VersionsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		versions = ""
	}
	if versions != "" {
		_ = os.MkdirAll(versions, 0755)
	}
	appVersion := updater.ActiveVersion()
	fallbackAppPath := ""
	if appVersion == "" {
		if previous := updater.PreviousVersion(); previous != "" && updater.CheckPayload(previous) == nil {
			appVersion = previous
		}
	}
	if appVersion == "" {
		appVersion = buildinfo.Version
		launcherPath, pathErr := os.Executable()
		if pathErr != nil {
			fmt.Fprintln(os.Stderr, pathErr)
			return 1
		}
		bootstrap := filepath.Join(filepath.Dir(launcherPath), "modlock-app.exe")
		if _, statErr := os.Stat(bootstrap); statErr != nil {
			fmt.Fprintln(os.Stderr, "ModLock app payload not found beside launcher:", bootstrap)
			return 1
		}
		if err = updater.InitializeBootstrap(bootstrap, appVersion); err != nil {
			// A second launcher may have raced the first bootstrap. Give the
			// winning process a short window to publish the initial pointer.
			if strings.Contains(err.Error(), "another ModLock process is updating") {
				deadline := time.Now().Add(5 * time.Second)
				for updater.ActiveVersion() == "" && time.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
				}
			}
			if updater.ActiveVersion() == "" {
				fmt.Fprintln(os.Stderr, "ModLock bootstrap failed; launching bundled app:", err)
				fallbackAppPath = bootstrap
			} else {
				appVersion = updater.ActiveVersion()
			}
		}
	}

	return runPayloadLoop(appVersion, fallbackAppPath, os.Args[1:], controlDir, productionDeps())
}

type launcherDeps struct {
	active, previous func() string
	appPath          func(string) (string, error)
	checkPayload     func(string) error
	rollback         func(string) error
	bundled          func() string
	launch           func(path, data string, skipUpdate, handoff bool, args []string) (int, int)
	stderr           *os.File
}

func productionDeps() launcherDeps {
	return launcherDeps{updater.ActiveVersion, updater.PreviousVersion, updater.AppPath, updater.CheckPayload, updater.AutomaticRollback, bundledPayload, launchAndWait, os.Stderr}
}

func runPayloadLoop(version, bundled string, args []string, data string, d launcherDeps) int {
	// Only failed startups are excluded from recovery. A healthy payload that
	// exits with 75 can be resumed after the replacement fails to start.
	tried := map[string]bool{}
	updateHandoff := false
	attempts, handoffs := 0, 0
	for {
		path := bundled
		if path == "" {
			if version == "" {
				fmt.Fprintln(d.stderr, "ModLock: нет пригодной версии приложения.")
				return 1
			}
			var err error
			path, err = d.appPath(version)
			if err != nil {
				fmt.Fprintln(d.stderr, "ModLock:", err)
				return 1
			}
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = filepath.Clean(path)
		}
		if tried[abs] {
			if current := d.active(); current != "" && !triedPath(d, current, tried) {
				version, bundled = current, ""
				continue
			}
			previous := d.previous()
			if previous != "" && !triedPath(d, previous, tried) && d.checkPayload(previous) == nil {
				version, bundled = previous, ""
				continue
			}
			if bundled == "" {
				bundled = d.bundled()
			}
			if bundled != "" && !triedPath(d, bundled, tried) {
				version = ""
				continue
			}
			fmt.Fprintln(d.stderr, "ModLock: payload был испробован и не запускается:", path)
			return 1
		}
		attempts++
		failedUpdateHandoff := false
		if _, err = os.Stat(path); err != nil {
			fmt.Fprintln(d.stderr, "ModLock: payload недоступен:", path, err)
			if updateHandoff {
				fmt.Fprintln(d.stderr, "ModLock: новая версия не запустилась; восстановлена предыдущая. Обновление не удалось.")
				failedUpdateHandoff = true
				updateHandoff = false
			}
		} else {
			isSelfUpdate := len(args) > 0 && args[0] == "self-update"
			code, exitCode := d.launch(path, data, attempts > 1, updateHandoff, args)
			if code == -1 {
				return exitCode
			}
			if code == 75 {
				handoffs++
				if handoffs > 4 {
					fmt.Fprintln(d.stderr, "ModLock: слишком много переключений версии.")
					return 1
				}
				updateHandoff = isSelfUpdate && !hasArg(args, "--check")
				version = d.active()
				bundled = ""
				if version == "" {
					fmt.Fprintln(d.stderr, "ModLock: переключение не выбрало активную версию.")
					return 1
				}
				continue
			}
			if updateHandoff {
				fmt.Fprintln(d.stderr, "ModLock: новая версия не запустилась; восстановлена предыдущая. Обновление не удалось.")
				failedUpdateHandoff = true
				updateHandoff = false
			}
		}

		tried[abs] = true
		// Startup failures share one recovery path. If another process changed
		// the active pointer, adopt its choice and never roll it back from stale state.
		if version != "" && d.active() == version {
			if err := d.rollback(version); err == nil {
				version = d.active()
				if failedUpdateHandoff {
					return 1
				}
				if version != "" {
					bundled = ""
					continue
				}
			} else {
				current := d.active()
				if current != "" && current != version && !triedPath(d, current, tried) {
					version, bundled = current, ""
					continue
				}
			}
		} else if current := d.active(); current != "" && current != version && !triedPath(d, current, tried) {
			version, bundled = current, ""
			continue
		}
		previous := d.previous()
		if previous != "" && previous != version && !triedPath(d, previous, tried) && d.checkPayload(previous) == nil {
			version, bundled = previous, ""
			continue
		}
		if bundled == "" {
			bundled = d.bundled()
		}
		if bundled != "" && !triedPath(d, bundled, tried) {
			version = ""
			continue
		}
		fmt.Fprintln(d.stderr, "ModLock: ни один payload не удалось запустить.")
		return 1
	}
}

func triedPath(d launcherDeps, path string, tried map[string]bool) bool {
	if versionPath, err := d.appPath(path); err == nil {
		path = versionPath
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	return tried[abs]
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func bundledPayload() string {
	launcherPath, err := os.Executable()
	if err != nil {
		return ""
	}
	path := filepath.Join(filepath.Dir(launcherPath), "modlock-app.exe")
	if _, err = os.Stat(path); err != nil {
		return ""
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, checkErr := exec.CommandContext(checkCtx, path, "--modlock-healthcheck").CombinedOutput(); checkErr != nil {
		fmt.Fprintln(os.Stderr, "ModLock: комплектный payload не прошёл проверку:", strings.TrimSpace(string(output)))
		return ""
	}
	return path
}

// launchAndWait returns code=-1 with exitCode set for a normal process exit,
// code=75 when the app requested a version handoff, or code=-2 on readiness timeout.
var startupTimeout = 10 * time.Second

func launchAndWait(path, data string, skipUpdate, handoffUpdate bool, args []string) (code, exitCode int) {
	ready := filepath.Join(data, fmt.Sprintf("ready-%d-%d", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(ready)
	cmd := exec.Command(path, args...)
	cmd.Dir, _ = os.Getwd()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	launcherPath, _ := os.Executable()
	cmd.Env = filteredEnv(os.Environ(), "MODLOCK_LAUNCHER", "MODLOCK_SKIP_UPDATE", "MODLOCK_READY_FILE", "MODLOCK_HANDOFF_UPDATE", "MODLOCK_LAUNCHER_DIR")
	cmd.Env = append(cmd.Env, "MODLOCK_LAUNCHER=1", "MODLOCK_READY_FILE="+ready, "MODLOCK_LAUNCHER_DIR="+filepath.Dir(launcherPath))
	if skipUpdate {
		cmd.Env = append(cmd.Env, "MODLOCK_SKIP_UPDATE=1")
	}
	if handoffUpdate {
		cmd.Env = append(cmd.Env, "MODLOCK_HANDOFF_UPDATE=1")
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ModLock launcher:", err)
		return -3, 1
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	timer := time.NewTimer(startupTimeout)
	defer timer.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			_ = os.Remove(ready)
			waitErr := <-finished
			if waitErr == nil {
				return -1, 0
			}
			if e, ok := waitErr.(*exec.ExitError); ok {
				if e.ExitCode() == 75 {
					return 75, -1
				}
				return -1, e.ExitCode()
			}
			fmt.Fprintln(os.Stderr, "ModLock launcher:", waitErr)
			return -1, 1
		}
		select {
		case err := <-finished:
			_, readyErr := os.Stat(ready)
			_ = os.Remove(ready)
			if readyErr == nil {
				if err == nil {
					return -1, 0
				}
				if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 75 {
					return 75, -1
				}
				if e, ok := err.(*exec.ExitError); ok {
					return -1, e.ExitCode()
				}
				fmt.Fprintln(os.Stderr, "ModLock launcher:", err)
				return -1, 1
			}
			if err == nil {
				return -3, 0
			}
			if e, ok := err.(*exec.ExitError); ok {
				if e.ExitCode() == 75 {
					return 75, -1
				}
				return -3, e.ExitCode()
			}
			fmt.Fprintln(os.Stderr, "ModLock launcher:", err)
			return -3, 1
		case <-timer.C:
			if _, err := os.Stat(ready); err == nil {
				continue
			}
			_ = cmd.Process.Kill()
			<-finished
			_ = os.Remove(ready)
			return -2, 1
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func filteredEnv(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		remove := false
		for _, k := range keys {
			if strings.EqualFold(name, k) {
				remove = true
				break
			}
		}
		if !remove {
			out = append(out, entry)
		}
	}
	return out
}
