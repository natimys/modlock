package providers

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"modlock/internal/lockfile"
)

type Detector struct {
	Client         *http.Client
	CurseForgeKey  string
	ModrinthBase   string
	CurseForgeBase string
}

func New() *Detector {
	return &Detector{Client: &http.Client{Timeout: 25 * time.Second}, CurseForgeKey: strings.TrimSpace(os.Getenv("CURSEFORGE_API_KEY"))}
}

type CurseForgeStatus struct {
	KeyFound  bool
	Available bool
	Message   string
}

func (d *Detector) CheckCurseForge(ctx context.Context) CurseForgeStatus {
	if strings.TrimSpace(d.CurseForgeKey) == "" {
		return CurseForgeStatus{Message: "CURSEFORGE_API_KEY не задан"}
	}
	_, base := d.bases()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/games/432", nil)
	req.Header.Set("x-api-key", d.CurseForgeKey)
	req.Header.Set("Accept", "application/json")
	resp, err := d.Client.Do(req)
	if err != nil {
		return CurseForgeStatus{KeyFound: true, Message: "не удалось подключиться: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		detail := strings.TrimSpace(string(body))
		if detail != "" {
			detail = ": " + detail
		}
		return CurseForgeStatus{KeyFound: true, Message: fmt.Sprintf("API отклонил запрос (HTTP %d)%s", resp.StatusCode, detail)}
	}
	return CurseForgeStatus{KeyFound: true, Available: true, Message: "ключ принят, API доступен"}
}

func (d *Detector) Detect(ctx context.Context, path string) (lockfile.ModEntry, error) {
	name := filepath.Base(path)
	sum, err := fileSHA1(path)
	if err != nil {
		return lockfile.ModEntry{}, err
	}
	if m, ok := d.modrinth(ctx, name, sum); ok {
		return m, nil
	}
	if d.CurseForgeKey != "" {
		if m, ok := d.curseforge(ctx, name, path); ok {
			return m, nil
		}
	}
	if id, version := jarIdentity(path); id != "" {
		return lockfile.ModEntry{ID: "minecraft:" + id, Version: version, Filename: name}, nil
	}
	return lockfile.ModEntry{Filename: name}, nil
}

func jarIdentity(path string) (string, string) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return "", ""
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name != "fabric.mod.json" {
			continue
		}
		rc, e := f.Open()
		if e != nil {
			continue
		}
		var meta struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		}
		e = json.NewDecoder(rc).Decode(&meta)
		rc.Close()
		if e == nil && meta.ID != "" {
			return meta.ID, meta.Version
		}
	}
	for _, f := range r.File {
		if f.Name != "META-INF/mods.toml" && f.Name != "META-INF/neoforge.mods.toml" {
			continue
		}
		rc, e := f.Open()
		if e != nil {
			continue
		}
		var meta struct {
			Mods []struct {
				ModID   string `toml:"modId"`
				Version string `toml:"version"`
			} `toml:"mods"`
		}
		_, e = toml.NewDecoder(rc).Decode(&meta)
		rc.Close()
		if e == nil && len(meta.Mods) > 0 && meta.Mods[0].ModID != "" {
			return meta.Mods[0].ModID, meta.Mods[0].Version
		}
	}
	return "", ""
}

func (d *Detector) bases() (string, string) {
	mr := d.ModrinthBase
	if mr == "" {
		mr = "https://api.modrinth.com"
	}
	cf := d.CurseForgeBase
	if cf == "" {
		cf = "https://api.curseforge.com"
	}
	return mr, cf
}

func RepoFallback(name string) lockfile.ModEntry {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	return RepoFallbackWithIdentity(name, "repo:"+base, "local")
}

func RepoFallbackWithIdentity(name, id, version string) lockfile.ModEntry {
	return lockfile.ModEntry{ID: id, Version: version, Filename: name, Source: "repo", Path: filepath.ToSlash(filepath.Join("files", "mods", name))}
}

func fileSHA1(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha1.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (d *Detector) modrinth(ctx context.Context, name, hash string) (lockfile.ModEntry, bool) {
	mr, _ := d.bases()
	req, _ := http.NewRequestWithContext(ctx, "GET", mr+"/v2/version_file/"+hash+"?algorithm=sha1", nil)
	req.Header.Set("User-Agent", "ModLock/1.0")
	resp, e := d.Client.Do(req)
	if e != nil {
		return lockfile.ModEntry{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return lockfile.ModEntry{}, false
	}
	var v struct {
		ID            string `json:"id"`
		ProjectID     string `json:"project_id"`
		VersionNumber string `json:"version_number"`
		Files         []struct {
			Hashes   map[string]string `json:"hashes"`
			URL      string            `json:"url"`
			Filename string            `json:"filename"`
			Primary  bool              `json:"primary"`
		} `json:"files"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return lockfile.ModEntry{}, false
	}
	for _, f := range v.Files {
		if f.Hashes["sha1"] == hash || f.Filename == name {
			version := v.VersionNumber
			if version == "" {
				version = v.ID
			}
			return lockfile.ModEntry{ID: "modrinth:" + v.ProjectID, Version: version, Filename: name, Source: "modrinth", ProjectID: v.ProjectID, VersionID: v.ID, URL: f.URL}, f.URL != ""
		}
	}
	return lockfile.ModEntry{}, false
}

func (d *Detector) curseforge(ctx context.Context, name, path string) (lockfile.ModEntry, bool) {
	f, e := os.Open(path)
	if e != nil {
		return lockfile.ModEntry{}, false
	}
	data, e := io.ReadAll(f)
	f.Close()
	if e != nil {
		return lockfile.ModEntry{}, false
	}
	clean := data[:0]
	for _, b := range data {
		if b != 9 && b != 10 && b != 13 && b != 32 {
			clean = append(clean, b)
		}
	}
	fp := murmur2(clean)
	body, _ := json.Marshal(map[string]any{"fingerprints": []uint32{fp}})
	_, cf := d.bases()
	req, _ := http.NewRequestWithContext(ctx, "POST", cf+"/v1/fingerprints/432", bytes.NewReader(body))
	req.Header.Set("x-api-key", d.CurseForgeKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, e := d.Client.Do(req)
	if e != nil {
		return lockfile.ModEntry{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return lockfile.ModEntry{}, false
	}
	var out struct {
		Data struct {
			ExactMatches []struct {
				File struct {
					ID          int64  `json:"id"`
					GameID      int64  `json:"gameId"`
					ModID       int64  `json:"modId"`
					DownloadURL string `json:"downloadUrl"`
				} `json:"file"`
			} `json:"exactMatches"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || len(out.Data.ExactMatches) == 0 {
		return lockfile.ModEntry{}, false
	}
	x := out.Data.ExactMatches[0].File
	if x.DownloadURL == "" {
		x.DownloadURL = d.curseforgeDownloadURL(ctx, cf, x.ModID, x.ID)
	}
	if x.DownloadURL == "" {
		return lockfile.ModEntry{}, false
	}
	return lockfile.ModEntry{ID: fmt.Sprintf("curseforge:%d", x.ModID), Version: fmt.Sprintf("%d", x.ID), Filename: name, Source: "curseforge", ModID: x.ModID, FileID: x.ID, URL: x.DownloadURL}, true
}

func (d *Detector) curseforgeDownloadURL(ctx context.Context, base string, modID, fileID int64) string {
	u := fmt.Sprintf("%s/v1/mods/%d/files/%d/download-url", base, modID, fileID)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("x-api-key", d.CurseForgeKey)
	req.Header.Set("Accept", "application/json")
	resp, err := d.Client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out struct {
		Data string `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	return out.Data
}

func murmur2(data []byte) uint32 {
	const m uint32 = 0x5bd1e995
	h := uint32(1) ^ uint32(len(data))
	i := 0
	for len(data)-i >= 4 {
		k := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
		k *= m
		k ^= k >> 24
		k *= m
		h *= m
		h ^= k
		i += 4
	}
	switch len(data) - i {
	case 3:
		h ^= uint32(data[i+2]) << 16
		fallthrough
	case 2:
		h ^= uint32(data[i+1]) << 8
		fallthrough
	case 1:
		h ^= uint32(data[i])
		h *= m
	}
	h ^= h >> 13
	h *= m
	h ^= h >> 15
	return h
}

func Copy(src, dst string) error {
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	if e = os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
		return e
	}
	out, e := os.Create(dst)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	ce := out.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return nil
}
func Download(ctx context.Context, c *http.Client, url, dst string) error {
	part := dst + ".part"
	defer os.Remove(part)
	req, e := http.NewRequestWithContext(ctx, "GET", url, nil)
	if e != nil {
		return e
	}
	resp, e := c.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPStatusError{Code: resp.StatusCode, Status: resp.Status}
	}
	if e = os.MkdirAll(filepath.Dir(dst), 0755); e != nil {
		return e
	}
	f, e := os.Create(part)
	if e != nil {
		return e
	}
	_, e = io.Copy(f, resp.Body)
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(part, dst)
}

type HTTPStatusError struct {
	Code   int
	Status string
}

func (e *HTTPStatusError) Error() string { return "HTTP " + e.Status }

func RetryableDownloadError(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Code == http.StatusRequestTimeout || statusErr.Code == http.StatusTooManyRequests || statusErr.Code >= 500
	}
	return true
}

type RetryProgress func(attempt, total int, previous error, wait time.Duration)

func DownloadWithRetry(ctx context.Context, c *http.Client, url, dst string, attempts int, progress RetryProgress) error {
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if progress != nil {
			progress(attempt, attempts, nil, 0)
		}
		last = Download(ctx, c, url, dst)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !RetryableDownloadError(last) || attempt == attempts {
			return last
		}
		wait := time.Duration(1<<(attempt-1)) * time.Second
		if progress != nil {
			progress(attempt, attempts, last, wait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
}
