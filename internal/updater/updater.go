package updater

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"modlock/internal/buildinfo"
)

const protocol = buildinfo.LoaderProtocol
const maxArchiveSize = 512 << 20

type Manifest struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Archive  string `json:"archive"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Protocol int    `json:"protocol"`
}
type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}
type cache struct {
	Checked time.Time `json:"checked"`
	Failed  bool      `json:"failed"`
}
type downloadStatusError struct{ status int }

func (e downloadStatusError) Error() string { return fmt.Sprintf("HTTP status %d", e.status) }
func retryableDownload(err error) bool {
	var statusErr downloadStatusError
	if errors.As(err, &statusErr) {
		return statusErr.status == http.StatusTooManyRequests || statusErr.status >= 500
	}
	var networkErr net.Error
	return errors.As(err, &networkErr) || errors.Is(err, io.ErrUnexpectedEOF)
}

func DataDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "ModLock"), nil
}
func VersionsDir() (string, error)  { d, e := DataDir(); return filepath.Join(d, "versions"), e }
func ActivePath() (string, error)   { d, e := DataDir(); return filepath.Join(d, "active.json"), e }
func PreviousPath() (string, error) { d, e := DataDir(); return filepath.Join(d, "previous.json"), e }

func readPointer(path string) string {
	b, e := os.ReadFile(path)
	if e != nil {
		return ""
	}
	var p struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	return p.Version
}
func writePointer(path, version string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		Version string `json:"version"`
	}{version})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return replaceFile(tmp, path)
}

func ActiveVersion() string   { p, _ := ActivePath(); return readPointer(p) }
func PreviousVersion() string { p, _ := PreviousPath(); return readPointer(p) }
func SetActiveVersion(version string) error {
	p, e := ActivePath()
	if e != nil {
		return e
	}
	return writePointer(p, version)
}

func InitializeBootstrap(src, version string) error {
	if ActiveVersion() != "" {
		return nil
	}
	dir, err := DataDir()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	release, err := acquireUpdateLock(dir)
	if err != nil {
		return err
	}
	defer release()
	if ActiveVersion() != "" {
		return nil
	}
	versions, err := VersionsDir()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(versions, 0755); err != nil {
		return err
	}
	cleanupTemps(versions)
	tmp, err := os.MkdirTemp(versions, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err = copyFile(src, filepath.Join(tmp, "modlock-app.exe")); err != nil {
		return err
	}
	final := filepath.Join(versions, version)
	if err = os.RemoveAll(final); err != nil {
		return err
	}
	if err = os.Rename(tmp, final); err != nil {
		return err
	}
	return SetActiveVersion(version)
}

func acquireUpdateLock(dir string) (func(), error) {
	lock := filepath.Join(dir, "update.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if stat, statErr := os.Stat(lock); statErr == nil && time.Since(stat.ModTime()) > 15*time.Minute {
			_ = os.Remove(lock)
			f, err = os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		}
	}
	if err != nil {
		return nil, errors.New("another ModLock process is updating")
	}
	_ = f.Close()
	return func() { _ = os.Remove(lock) }, nil
}
func AppPath(version string) (string, error) {
	d, e := VersionsDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(d, version, "modlock-app.exe"), nil
}

func AutomaticCheck(ctx context.Context) (bool, error) { return update(ctx, false, false) }
func ManualUpdate(ctx context.Context, checkOnly bool) (bool, error) {
	return update(ctx, true, checkOnly)
}

func update(ctx context.Context, force, checkOnly bool) (bool, error) {
	if buildinfo.Version == "dev" || buildinfo.ReleaseRepo == "" {
		if force {
			return false, errors.New("self-update is unavailable in this local build (release repository is not configured)")
		}
		return false, nil
	}
	if _, err := parseVersion(buildinfo.Version); err != nil {
		return false, fmt.Errorf("invalid built-in version: %w", err)
	}
	cleanupInterrupted()
	if !force && !cacheDue() {
		return false, nil
	}
	manifest, archiveURL, err := fetchManifest(ctx)
	if err != nil {
		setCache(true)
		return false, err
	}
	setCache(false)
	cmp, err := compareVersions(manifest.Version, buildinfo.Version)
	if err != nil {
		return false, err
	}
	if cmp <= 0 {
		return false, nil
	}
	if checkOnly {
		return true, nil
	}
	if err = install(ctx, manifest, archiveURL); err != nil {
		setCache(true)
		return false, err
	}
	return true, nil
}

func cleanupInterrupted() {
	dir, err := DataDir()
	if err != nil || os.MkdirAll(dir, 0755) != nil {
		return
	}
	release, err := acquireUpdateLock(dir)
	if err != nil {
		return
	}
	defer release()
	versions, err := VersionsDir()
	if err == nil {
		cleanupTemps(versions)
	}
}

func cacheDue() bool {
	d, e := DataDir()
	if e != nil {
		return true
	}
	b, e := os.ReadFile(filepath.Join(d, "update-cache.json"))
	if e != nil {
		return true
	}
	var c cache
	if json.Unmarshal(b, &c) != nil {
		return true
	}
	limit := 24 * time.Hour
	if c.Failed {
		limit = time.Hour
	}
	return time.Since(c.Checked) >= limit
}
func setCache(failed bool) {
	d, e := DataDir()
	if e != nil {
		return
	}
	_ = os.MkdirAll(d, 0755)
	b, _ := json.Marshal(cache{time.Now().UTC(), failed})
	_ = os.WriteFile(filepath.Join(d, "update-cache.json"), b, 0600)
}

func fetchManifest(ctx context.Context) (Manifest, string, error) {
	parts := strings.Split(buildinfo.ReleaseRepo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Manifest{}, "", errors.New("release repository must be owner/repo")
	}
	api := "https://api.github.com/repos/" + parts[0] + "/" + parts[1] + "/releases/latest"
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	req, e := http.NewRequestWithContext(short, "GET", api, nil)
	if e != nil {
		return Manifest{}, "", e
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ModLock-Updater")
	resp, e := client.Do(req)
	if e != nil {
		return Manifest{}, "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Manifest{}, "", fmt.Errorf("GitHub Releases API: %s", resp.Status)
	}
	var rel release
	if e = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&rel); e != nil {
		return Manifest{}, "", e
	}
	if rel.Draft || rel.Prerelease {
		return Manifest{}, "", errors.New("latest release is not stable")
	}
	var manifestURL string
	for _, a := range rel.Assets {
		if a.Name == "manifest.json" {
			manifestURL = a.URL
			break
		}
	}
	if manifestURL == "" {
		return Manifest{}, "", errors.New("latest release has no manifest.json asset")
	}
	mreq, e := http.NewRequestWithContext(short, "GET", manifestURL, nil)
	if e != nil {
		return Manifest{}, "", e
	}
	mreq.Header.Set("Accept", "application/octet-stream")
	mreq.Header.Set("User-Agent", "ModLock-Updater")
	mresp, e := client.Do(mreq)
	if e != nil {
		return Manifest{}, "", e
	}
	defer mresp.Body.Close()
	if mresp.StatusCode != 200 {
		return Manifest{}, "", fmt.Errorf("manifest download: %s", mresp.Status)
	}
	var m Manifest
	if e = json.NewDecoder(io.LimitReader(mresp.Body, 1<<20)).Decode(&m); e != nil {
		return Manifest{}, "", e
	}
	if m.Version != strings.TrimPrefix(rel.Tag, "v") {
		return Manifest{}, "", errors.New("release tag and manifest version do not match")
	}
	if _, e = parseVersion(m.Version); e != nil {
		return Manifest{}, "", e
	}
	if m.Protocol != protocol {
		return Manifest{}, "", fmt.Errorf("loader protocol %d is incompatible with required protocol %d", m.Protocol, protocol)
	}
	wantPlatform := runtime.GOOS + "-" + runtime.GOARCH
	if m.Platform != wantPlatform {
		return Manifest{}, "", fmt.Errorf("release platform %q does not match %q", m.Platform, wantPlatform)
	}
	if filepath.Base(m.Archive) != m.Archive || m.Archive == "" || m.Size <= 0 || m.Size > maxArchiveSize || len(m.SHA256) != 64 {
		return Manifest{}, "", errors.New("invalid release manifest")
	}
	if _, e = hex.DecodeString(m.SHA256); e != nil {
		return Manifest{}, "", errors.New("invalid manifest SHA-256")
	}
	var zipURL string
	for _, a := range rel.Assets {
		if a.Name == m.Archive {
			zipURL = a.URL
			break
		}
	}
	if zipURL == "" {
		return Manifest{}, "", errors.New("release archive is missing")
	}
	return m, zipURL, nil
}

func install(ctx context.Context, m Manifest, url string) error {
	dir, e := DataDir()
	if e != nil {
		return e
	}
	if e = os.MkdirAll(dir, 0755); e != nil {
		return e
	}
	release, e := acquireUpdateLock(dir)
	if e != nil {
		return e
	}
	defer release()
	versions, e := VersionsDir()
	if e != nil {
		return e
	}
	if e = os.MkdirAll(versions, 0755); e != nil {
		return e
	}
	cleanupTemps(versions)
	tmpZip := filepath.Join(versions, "download-"+m.Version+".zip.part")
	defer os.Remove(tmpZip)
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !retryableDownload(last) {
			break
		}
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 300 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		last = download(ctx, url, tmpZip, m.Size)
		if last == nil {
			break
		}
	}
	if last != nil {
		return fmt.Errorf("download release: %w", last)
	}
	archive, e := os.Open(tmpZip)
	if e != nil {
		return e
	}
	h := sha256.New()
	n, hashErr := io.Copy(h, archive)
	closeErr := archive.Close()
	if hashErr != nil {
		return hashErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != m.Size {
		return errors.New("release archive size mismatch")
	}
	if hex.EncodeToString(h.Sum(nil)) != strings.ToLower(m.SHA256) {
		return errors.New("release archive SHA-256 mismatch")
	}
	tmpDir, e := os.MkdirTemp(versions, ".install-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(tmpDir)
	if e = extractApp(tmpZip, tmpDir); e != nil {
		return e
	}
	exe := filepath.Join(tmpDir, "modlock-app.exe")
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, exe, "--modlock-healthcheck")
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf("new version health check failed: %w: %s", runErr, strings.TrimSpace(string(out)))
	}
	final := filepath.Join(versions, m.Version)
	if _, e = os.Stat(final); e == nil {
		installed := filepath.Join(final, "modlock-app.exe")
		checkExisting, cancelExisting := context.WithTimeout(ctx, 10*time.Second)
		checkCmd := exec.CommandContext(checkExisting, installed, "--modlock-healthcheck")
		out, checkErr := checkCmd.CombinedOutput()
		cancelExisting()
		if checkErr != nil {
			return fmt.Errorf("version %s already exists but failed its health check: %w: %s", m.Version, checkErr, strings.TrimSpace(string(out)))
		}
		_ = os.RemoveAll(tmpDir)
	} else if !os.IsNotExist(e) {
		return e
	} else if e = os.Rename(tmpDir, final); e != nil {
		return e
	}
	active, _ := ActivePath()
	previous, _ := PreviousPath()
	old := readPointer(active)
	if old == "" {
		old = buildinfo.Version
	}
	if e = writePointer(previous, old); e != nil {
		return e
	}
	if e = writePointer(active, m.Version); e != nil {
		return e
	}
	return nil
}

func download(ctx context.Context, url, path string, size int64) error {
	dlCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, e := http.NewRequestWithContext(dlCtx, "GET", url, nil)
	if e != nil {
		return e
	}
	req.Header.Set("User-Agent", "ModLock-Updater")
	resp, e := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return downloadStatusError{status: resp.StatusCode}
	}
	if resp.ContentLength >= 0 && resp.ContentLength != size {
		return fmt.Errorf("unexpected content length %d", resp.ContentLength)
	}
	out, e := os.Create(path)
	if e != nil {
		return e
	}
	reader := io.LimitReader(resp.Body, size+1)
	buf := make([]byte, 64*1024)
	var n int64
	lastPercent := -1
	for {
		nr, readErr := reader.Read(buf)
		if nr > 0 {
			nw, writeErr := out.Write(buf[:nr])
			n += int64(nw)
			if writeErr != nil {
				e = writeErr
				break
			}
			if nw != nr {
				e = io.ErrShortWrite
				break
			}
			percent := int(n * 100 / size)
			if percent != lastPercent {
				fmt.Fprintf(os.Stderr, "\rModLock: скачивание обновления %d%%", percent)
				lastPercent = percent
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			e = readErr
			break
		}
	}
	closeErr := out.Close()
	fmt.Fprintln(os.Stderr)
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if n != size {
		if n < size {
			return io.ErrUnexpectedEOF
		}
		return fmt.Errorf("downloaded %d bytes, expected %d", n, size)
	}
	return nil
}

func extractApp(zipPath, dest string) error {
	r, e := zip.OpenReader(zipPath)
	if e != nil {
		return e
	}
	defer r.Close()
	found := false
	for _, f := range r.File {
		clean := filepath.Clean(filepath.FromSlash(f.Name))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return errors.New("unsafe path in release archive")
		}
		if f.Name != "modlock-app.exe" {
			return fmt.Errorf("unexpected file %q in release archive", f.Name)
		}
		if f.Mode()&os.ModeSymlink != 0 || f.FileInfo().IsDir() || !f.Mode().IsRegular() || f.UncompressedSize64 == 0 || f.UncompressedSize64 > maxArchiveSize {
			return errors.New("release application is not a regular file")
		}
		rc, er := f.Open()
		if er != nil {
			return er
		}
		target := filepath.Join(dest, "modlock-app.exe")
		out, er := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if er == nil {
			var written int64
			written, er = io.Copy(out, io.LimitReader(rc, maxArchiveSize+1))
			if er == nil && uint64(written) != f.UncompressedSize64 {
				er = errors.New("application size does not match ZIP metadata")
			}
			closeErr := out.Close()
			if er == nil {
				er = closeErr
			}
		}
		_ = rc.Close()
		if er != nil {
			return er
		}
		found = true
	}
	if !found {
		return errors.New("release archive does not contain modlock-app.exe")
	}
	return nil
}

func cleanupTemps(dir string) {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".install-") || strings.HasPrefix(entry.Name(), "download-") && strings.HasSuffix(entry.Name(), ".part") {
			_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		_ = os.Remove(dst)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	return nil
}

func Rollback() error {
	dir, err := DataDir()
	if err != nil {
		return err
	}
	release, err := acquireUpdateLock(dir)
	if err != nil {
		return err
	}
	defer release()
	active, e := ActivePath()
	if e != nil {
		return e
	}
	previous, e := PreviousPath()
	if e != nil {
		return e
	}
	v := readPointer(previous)
	if v == "" {
		return errors.New("no previous application version is available")
	}
	if _, e = parseVersion(v); e != nil {
		return e
	}
	current := readPointer(active)
	if e = writePointer(active, v); e != nil {
		return e
	}
	if current != "" {
		if e = writePointer(previous, current); e != nil {
			return e
		}
	}
	return nil
}
