package main

import (
	"context"
	"fmt"
	"os"

	"modlock/internal/app"
	"modlock/internal/buildinfo"
	"modlock/internal/updater"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--modlock-healthcheck" {
		if buildinfo.Version == "" || buildinfo.LoaderProtocol != 1 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctx := context.Background()
	manualUpdateCommand := len(os.Args) > 1 && os.Args[1] == "self-update"
	if os.Getenv("MODLOCK_SKIP_UPDATE") == "" && !manualUpdateCommand {
		if updated, updateErr := updater.AutomaticCheck(ctx); updateErr != nil {
			fmt.Fprintln(os.Stderr, "ModLock: update check failed:", updateErr)
		} else if updated {
			if os.Getenv("MODLOCK_LAUNCHER") == "1" {
				os.Exit(75)
			}
			fmt.Fprintln(os.Stderr, "ModLock: новая версия установлена; для её запуска откройте ModLock через modlock.exe.")
		}
	}
	if ready := os.Getenv("MODLOCK_READY_FILE"); ready != "" {
		_ = os.WriteFile(ready, []byte(buildinfo.Version), 0600)
	}
	var err error
	if len(os.Args) == 1 {
		err = app.TUI(ctx)
	} else {
		if os.Args[1] == "self-update" && os.Getenv("MODLOCK_HANDOFF_UPDATE") == "1" {
			if len(os.Args) == 3 && os.Args[2] == "--rollback" {
				fmt.Fprintln(os.Stdout, "Откат выполнен.")
			} else {
				fmt.Printf("ModLock обновлён до %s.\n", buildinfo.Version)
			}
			return
		}
		switch os.Args[1] {
		case "sync":
			err = app.Sync(ctx)
		case "diff":
			err = app.Diff()
		case "push":
			err = app.Push(ctx, os.Args[2:])
		case "init":
			err = app.Init(ctx, os.Stdin)
		case "add":
			err = app.Add(ctx, os.Args[2:], os.Stdin)
		case "ignore":
			err = app.Ignore(ctx, os.Args[2:], os.Stdin)
		case "revert":
			err = app.Revert(os.Args[2:], os.Stdin)
		case "version":
			fmt.Printf("ModLock %s\ncommit: %s\nbuilt: %s\nreleases: %s\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date, buildinfo.ReleaseRepo)
		case "self-update":
			rollback := len(os.Args) == 3 && os.Args[2] == "--rollback"
			checkOnly := len(os.Args) == 3 && os.Args[2] == "--check"
			if len(os.Args) > 3 || (len(os.Args) == 3 && !checkOnly && !rollback) {
				err = fmt.Errorf("usage: modlock self-update [--check|--rollback]")
				break
			}
			if rollback {
				err = updater.Rollback()
				if err == nil && os.Getenv("MODLOCK_LAUNCHER") == "1" {
					os.Exit(75)
				}
				if err == nil {
					fmt.Fprintln(os.Stderr, "Предыдущая версия выбрана. Запустите через modlock.exe для переключения.")
				}
				break
			}
			var found bool
			found, err = updater.ManualUpdate(ctx, checkOnly)
			if err == nil {
				if checkOnly {
					if found {
						fmt.Fprintln(os.Stdout, "Доступна новая версия.")
					} else {
						fmt.Fprintln(os.Stdout, "Обновлений нет.")
					}
				} else if found && os.Getenv("MODLOCK_LAUNCHER") == "1" {
					os.Exit(75)
				} else if found {
					fmt.Fprintln(os.Stderr, "Новая версия установлена. Запустите через modlock.exe для переключения.")
				} else {
					fmt.Fprintln(os.Stdout, "ModLock уже обновлён.")
				}
			}
		default:
			fmt.Fprintln(os.Stderr, "Usage: modlock [sync|diff|push|init|add|ignore|revert|version|self-update]")
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
