package app

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"modlock/internal/lockfile"
)

// Migrate upgrades a schema 1 pack lock to schema 2. Content hashes are taken
// from the pack's local files; component versions are supplied by the author
// because schema 1 did not record them.
func Migrate(args []string) error {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	name := flags.String("name", "", "pack name (required)")
	version := flags.String("version", "", "pack version (required)")
	minecraft := flags.String("minecraft", "", "exact Minecraft version (required)")
	loaderID := flags.String("loader-id", "", "profile component ID, for example net.fabricmc.fabric-loader")
	loaderVersion := flags.String("loader-version", "", "exact loader version; requires --loader-id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("usage: modlock migrate --name NAME --version VERSION --minecraft VERSION [--loader-id ID --loader-version VERSION]")
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*version) == "" || strings.TrimSpace(*minecraft) == "" {
		return fmt.Errorf("--name, --version, and --minecraft are required")
	}
	if (*loaderID == "") != (*loaderVersion == "") {
		return fmt.Errorf("--loader-id and --loader-version must be provided together")
	}

	root, err := FindRoot(true)
	if err != nil {
		return err
	}
	path, err := lockfile.FindPath(root)
	if err != nil {
		return fmt.Errorf("find schema 1 lock: %w", err)
	}
	lock, err := lockfile.Read(path)
	if err != nil {
		return err
	}
	if lock.Schema != 1 {
		return fmt.Errorf("%s uses schema %d; migrate only accepts schema 1", path, lock.Schema)
	}
	backup := path + ".schema1.bak"
	if _, err := os.Lstat(backup); err == nil {
		return fmt.Errorf("backup already exists: %s; move it before retrying", backup)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check backup path: %w", err)
	}

	lock.Pack.Name = strings.TrimSpace(*name)
	lock.Pack.Version = strings.TrimSpace(*version)
	lock.Pack.Components = []lockfile.Component{{ID: "net.minecraft", Version: strings.TrimSpace(*minecraft)}}
	if *loaderID != "" {
		lock.Pack.Components = append(lock.Pack.Components, lockfile.Component{ID: strings.TrimSpace(*loaderID), Version: strings.TrimSpace(*loaderVersion)})
	}
	for i := range lock.Mods {
		mod := &lock.Mods[i]
		candidates := []string{}
		if mod.Source == "repo" && mod.Path != "" {
			candidates = append(candidates, mod.Path)
		}
		candidates = append(candidates, filepath.ToSlash(filepath.Join(lock.Pack.ModsDir, mod.Filename)))
		var hash string
		var hashErr error
		for _, rel := range candidates {
			var full string
			full, hashErr = lockfile.ResolveWithin(root, rel)
			if hashErr != nil {
				continue
			}
			hash, hashErr = sha256File(full)
			if hashErr == nil {
				break
			}
		}
		if hashErr != nil || hash == "" {
			return fmt.Errorf("cannot hash %s from local pack files; place it at %s (or its repository path) and retry", mod.Filename, filepath.Join(lock.Pack.ModsDir, mod.Filename))
		}
		mod.SHA256 = hash
	}
	lock.Schema = 2
	if err := copyFileExclusive(path, backup); err != nil {
		return fmt.Errorf("preserve schema 1 lock: %w", err)
	}
	if err := lockfile.Write(path, lock); err != nil {
		return fmt.Errorf("migration failed; original schema 1 lock is preserved at %s: %w", backup, err)
	}
	fmt.Printf("Migrated %s to schema 2. Schema 1 backup: %s\n", path, backup)
	fmt.Println("Review pack components and file policies, then publish the updated lock.")
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func copyFileExclusive(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return closeErr
	}
	return nil
}
