// modlock.exe is intentionally small and stable. It owns process handoff and
// never replaces its own executable while Windows has it open.
package main

import (
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

	for handoff := 0; handoff < 3; handoff++ {
		appPath := fallbackAppPath
		if updater.ActiveVersion() != "" {
			fallbackAppPath = ""
		}
		if fallbackAppPath == "" {
			appPath, err = updater.AppPath(appVersion)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		if _, err = os.Stat(appPath); err != nil {
			fmt.Fprintln(os.Stderr, "ModLock version is missing:", appVersion)
			return 1
		}
		skipUpdateAction := handoff > 0 && len(os.Args) > 1 && os.Args[1] == "self-update"
		code, exitCode := launchAndWait(appPath, controlDir, handoff > 0, skipUpdateAction)
		if code != 75 && code != -2 {
			return exitCode
		}
		if code == 75 {
			appVersion = updater.ActiveVersion()
			if appVersion == "" {
				fmt.Fprintln(os.Stderr, "ModLock update did not set an active version")
				return 1
			}
			continue
		}
		if code == -2 {
			old := updater.PreviousVersion()
			if old != "" && old != appVersion {
				fmt.Fprintln(os.Stderr, "ModLock: новая версия не подтвердила запуск; восстановлена предыдущая.")
				if err = updater.Rollback(); err != nil {
					fmt.Fprintln(os.Stderr, "rollback:", err)
					return 1
				}
				appVersion = old
				continue
			}
			fmt.Fprintln(os.Stderr, "ModLock: приложение не подтвердило запуск за 10 секунд.")
			return 1
		}
		return code
	}
	fmt.Fprintln(os.Stderr, "ModLock: too many update handoffs")
	return 1
}

// launchAndWait returns code=-1 with exitCode set for a normal process exit,
// code=75 when the app requested a version handoff, or code=-2 on readiness timeout.
func launchAndWait(path, data string, skipUpdate, skipUpdateAction bool) (code, exitCode int) {
	ready := filepath.Join(data, fmt.Sprintf("ready-%d-%d", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(ready)
	cmd := exec.Command(path, os.Args[1:]...)
	cmd.Dir, _ = os.Getwd()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = filteredEnv(os.Environ(), "MODLOCK_LAUNCHER", "MODLOCK_SKIP_UPDATE", "MODLOCK_READY_FILE", "MODLOCK_HANDOFF_UPDATE")
	cmd.Env = append(cmd.Env, "MODLOCK_LAUNCHER=1", "MODLOCK_READY_FILE="+ready)
	if skipUpdate {
		cmd.Env = append(cmd.Env, "MODLOCK_SKIP_UPDATE=1")
	}
	if skipUpdateAction {
		cmd.Env = append(cmd.Env, "MODLOCK_HANDOFF_UPDATE=1")
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ModLock launcher:", err)
		return -1, 1
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	timer := time.NewTimer(10 * time.Second)
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
				return e.ExitCode(), e.ExitCode()
			}
			fmt.Fprintln(os.Stderr, "ModLock launcher:", waitErr)
			return -1, 1
		}
		select {
		case err := <-finished:
			_ = os.Remove(ready)
			if err == nil {
				return -1, 0
			}
			if e, ok := err.(*exec.ExitError); ok {
				if e.ExitCode() == 75 {
					return 75, -1
				}
				return e.ExitCode(), e.ExitCode()
			}
			fmt.Fprintln(os.Stderr, "ModLock launcher:", err)
			return -1, 1
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
