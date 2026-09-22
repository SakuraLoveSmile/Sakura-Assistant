package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed installer/install.sh
var installerScript embed.FS

const (
	agentRepo                   = "SakuraLoveSmile/Sakura-Assistant"
	agentMaxAssetBytes    int64 = 128 * 1024 * 1024
	agentMaxMetadataBytes int64 = 1024 * 1024
)

var agentVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var agentHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type agentManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Version       string       `json:"version"`
	Assets        []agentAsset `json:"assets"`
}
type agentAsset struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Downloads/cache mutations are serialized, but serving a verified file is not.
// Leases protect actively served files from eviction, including their disk usage.
// The gate is cancellation aware and has no per-request/per-version map growth.
type agentCache struct {
	dir         string
	max         int64
	gate        chan struct{}
	mu          sync.Mutex
	refs        map[string]int
	initialized bool
}

func newAgentCache(dir string, max int64) *agentCache {
	return &agentCache{dir: dir, max: max, gate: make(chan struct{}, 1), refs: make(map[string]int)}
}
func (c *agentCache) path(key string) string { return filepath.Join(c.dir, key) }
func validAgentVersion(v string) bool        { return len(v) <= 64 && agentVersionRE.MatchString(v) }
func validAgentAssetName(name string) bool {
	if name == "agent-manifest.json" || name == "agent-SHA256SUMS.txt" {
		return true
	}
	for _, arch := range []string{"amd64", "arm64"} {
		const prefix = "assistant-agent_"
		suffix := "_linux_" + arch
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
			return validAgentVersion(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix))
		}
	}
	return false
}
func managedAgentCacheKey(key string) bool {
	version, asset, ok := strings.Cut(key, "_")
	return ok && validAgentVersion(version) && validAgentAssetName(asset)
}
func parseAgentManifest(b []byte, version string) (*agentManifest, error) {
	var m agentManifest
	if int64(len(b)) > agentMaxMetadataBytes || !validAgentVersion(version) || json.Unmarshal(b, &m) != nil || m.SchemaVersion != 1 || m.Version != version || len(m.Assets) != 2 {
		return nil, errors.New("invalid agent manifest")
	}
	seen := map[string]bool{}
	for _, x := range m.Assets {
		if x.OS != "linux" || (x.Arch != "amd64" && x.Arch != "arm64") || seen[x.Arch] || x.Size <= 0 || x.Size > agentMaxAssetBytes || !agentHashRE.MatchString(x.SHA256) || x.Name != "assistant-agent_"+version+"_linux_"+x.Arch {
			return nil, errors.New("invalid agent manifest asset")
		}
		seen[x.Arch] = true
	}
	return &m, nil
}
func validAgentChecksums(b []byte, m *agentManifest) bool {
	var expected strings.Builder
	for _, x := range m.Assets {
		fmt.Fprintf(&expected, "%s  %s\n", x.SHA256, x.Name)
	}
	return string(b) == expected.String()
}
func hexDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (a *app) handleAgentInstallScript(w http.ResponseWriter, r *http.Request) {
	b, err := installerScript.ReadFile("installer/install.sh")
	if err != nil {
		errInternal(w)
		return
	}
	w.Header().Set("Content-Type", "application/x-sh; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}
func (a *app) handleAgentStable(w http.ResponseWriter, r *http.Request) {
	if !validAgentVersion(a.cfg.agentVersion) {
		agentReleaseErr(w)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	a.serveAgentAsset(w, r, a.cfg.agentVersion, "agent-manifest.json")
}
func (a *app) handleAgentAsset(w http.ResponseWriter, r *http.Request) {
	version, asset := r.PathValue("version"), r.PathValue("asset")
	if !validAgentVersion(version) || !validAgentAssetName(asset) {
		errNotFound(w)
		return
	}
	if strings.HasPrefix(asset, "assistant-agent_") && asset != "assistant-agent_"+version+"_linux_amd64" && asset != "assistant-agent_"+version+"_linux_arm64" {
		errNotFound(w)
		return
	}
	a.serveAgentAsset(w, r, version, asset)
}
func (a *app) serveAgentAsset(w http.ResponseWriter, r *http.Request, version, asset string) {
	f, release, err := a.openAgentAsset(r.Context(), version, asset)
	if err != nil {
		agentReleaseErr(w)
		return
	}
	defer release()
	info, err := f.Stat()
	if err != nil {
		agentReleaseErr(w)
		return
	}
	switch asset {
	case "agent-manifest.json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	case "agent-SHA256SUMS.txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, asset, info.ModTime(), f)
}

// openAgentAsset never returns a partially downloaded or unverified file.
func (a *app) openAgentAsset(ctx context.Context, version, asset string) (*os.File, func(), error) {
	if !validAgentVersion(version) || !validAgentAssetName(asset) {
		return nil, nil, errors.New("invalid asset")
	}
	c := a.agentCache
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := c.prepare(); err != nil {
		return nil, nil, err
	}
	manifestName := version + "_agent-manifest.json"
	mb, err := a.agentMetadata(ctx, version, "agent-manifest.json", func(b []byte) bool { _, e := parseAgentManifest(b, version); return e == nil })
	if err != nil {
		return nil, nil, err
	}
	manifest, err := parseAgentManifest(mb, version)
	if err != nil {
		return nil, nil, err
	}
	key := version + "_" + asset
	switch asset {
	case "agent-manifest.json":
		key = manifestName
	case "agent-SHA256SUMS.txt":
		if _, err = a.agentMetadata(ctx, version, asset, func(b []byte) bool { return validAgentChecksums(b, manifest) }); err != nil {
			return nil, nil, err
		}
	default:
		var expected *agentAsset
		for i := range manifest.Assets {
			if manifest.Assets[i].Name == asset {
				expected = &manifest.Assets[i]
				break
			}
		}
		if expected == nil {
			return nil, nil, errors.New("asset not in manifest")
		}
		if err = a.agentBinary(ctx, version, *expected); err != nil {
			return nil, nil, err
		}
	}
	// No eviction/download can intervene before the lease is registered.
	f, err := os.Open(c.path(key))
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	c.refs[key]++
	c.mu.Unlock()
	release := func() {
		_ = f.Close()
		c.mu.Lock()
		c.refs[key]--
		if c.refs[key] == 0 {
			delete(c.refs, key)
		}
		c.mu.Unlock()
	}
	return f, release, nil
}

func (c *agentCache) prepare() error {
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(c.dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("cache is not a directory")
	}
	if !c.initialized {
		entries, err := os.ReadDir(c.dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".agent-download-") && e.Type().IsRegular() {
				if err := os.Remove(c.path(e.Name())); err != nil {
					return err
				}
			}
		}
		c.initialized = true
	}
	return c.makeRoom(0)
}

// makeRoom reserves space BEFORE temporary writes; active downloads are serialized.
// Unknown user files count against capacity but are never deleted.
func (c *agentCache) makeRoom(extra int64) error {
	if extra > c.max || c.max <= 0 {
		return errors.New("cache capacity too small")
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	type candidate struct {
		name  string
		size  int64
		mtime time.Time
	}
	var total int64
	var candidates []candidate
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		if managedAgentCacheKey(e.Name()) && c.refs[e.Name()] == 0 {
			candidates = append(candidates, candidate{e.Name(), info.Size(), info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mtime.Before(candidates[j].mtime) })
	for _, f := range candidates {
		if total+extra <= c.max {
			break
		}
		if err := os.Remove(c.path(f.name)); err != nil {
			return err
		}
		total -= f.size
	}
	if total+extra > c.max {
		return errors.New("cache capacity exhausted by active files")
	}
	return nil
}
func (c *agentCache) discard(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refs[key] != 0 {
		return errors.New("invalid cache entry currently served")
	}
	err := os.Remove(c.path(key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func readSmallAgentFile(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > agentMaxMetadataBytes {
		return nil, errors.New("invalid metadata cache file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, agentMaxMetadataBytes+1))
	if int64(len(b)) > agentMaxMetadataBytes {
		return nil, errors.New("metadata too large")
	}
	return b, err
}
func (a *app) agentMetadata(ctx context.Context, version, name string, valid func([]byte) bool) ([]byte, error) {
	c := a.agentCache
	key := version + "_" + name
	if b, err := readSmallAgentFile(c.path(key)); err == nil && valid(b) {
		return b, nil
	}
	if err := c.discard(key); err != nil {
		return nil, err
	}
	resp, err := a.fetchAgentAsset(ctx, version, name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > agentMaxMetadataBytes {
		return nil, errors.New("metadata too large")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, agentMaxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > agentMaxMetadataBytes || !valid(b) {
		return nil, errors.New("invalid metadata")
	}
	if err := c.makeRoom(int64(len(b))); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(c.dir, ".agent-download-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(b); err != nil {
		return nil, err
	}
	if err = tmp.Sync(); err != nil {
		return nil, err
	}
	if err = tmp.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(tmp.Name(), c.path(key)); err != nil {
		return nil, err
	}
	return b, nil
}
func validAgentBinary(path string, x agentAsset) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != x.Size {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, x.Size+1))
	return err == nil && n == x.Size && hex.EncodeToString(h.Sum(nil)) == x.SHA256
}
func (a *app) agentBinary(ctx context.Context, version string, x agentAsset) error {
	c := a.agentCache
	key := version + "_" + x.Name
	if validAgentBinary(c.path(key), x) {
		return nil
	}
	if err := c.discard(key); err != nil {
		return err
	}
	if err := c.makeRoom(x.Size); err != nil {
		return err
	}
	resp, err := a.fetchAgentAsset(ctx, version, x.Name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 && resp.ContentLength != x.Size {
		return errors.New("asset size mismatch")
	}
	tmp, err := os.CreateTemp(c.dir, ".agent-download-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	if _, err = io.CopyN(io.MultiWriter(tmp, h), resp.Body, x.Size); err != nil {
		return errors.New("incomplete asset")
	}
	// Probe an additional byte WITHOUT storing it, preserving the disk reservation.
	var extra [1]byte
	if n, err := io.ReadFull(resp.Body, extra[:]); n != 0 || err != io.EOF {
		return errors.New("asset size mismatch")
	}
	if hex.EncodeToString(h.Sum(nil)) != x.SHA256 {
		return errors.New("asset digest mismatch")
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path(key))
}
func (a *app) fetchAgentAsset(ctx context.Context, version, asset string) (*http.Response, error) {
	endpoint := "https://github.com/" + agentRepo + "/releases/download/v" + version + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.githubClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("release upstream status %d", resp.StatusCode)
	}
	return resp, nil
}
func agentRedirectAllowed(req *http.Request, via []*http.Request) error {
	// GitHub's signed CDN URLs intentionally contain a query string. It is never logged.
	if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" || (req.URL.Port() != "" && req.URL.Port() != "443") {
		return errors.New("untrusted release redirect")
	}
	switch strings.ToLower(req.URL.Hostname()) {
	case "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com", "github-releases.githubusercontent.com":
		return nil
	default:
		return errors.New("untrusted release redirect")
	}
}
func (a *app) githubClient() *http.Client {
	if a.agentHTTP != nil {
		return a.agentHTTP
	}
	return &http.Client{Timeout: 120 * time.Second, CheckRedirect: agentRedirectAllowed}
}
func agentReleaseErr(w http.ResponseWriter) {
	writeErr(w, 502, "agent_release_unavailable", "Agent 发布资产暂不可用，请检查版本、上游连接和缓存空间")
}
func (a *app) agentStatusAuth(next func(http.ResponseWriter, *http.Request, *sourceRow)) http.HandlerFunc {
	return a.sourceKeyAuth(func(w http.ResponseWriter, r *http.Request, src *sourceRow) {
		if src.Kind != "device" {
			errForbidden(w, "forbidden", "该来源不支持 Agent 状态查询")
			return
		}
		next(w, r, src)
	})
}
func (a *app) handleAgentStatus(w http.ResponseWriter, _ *http.Request, src *sourceRow) {
	var seen, ver any
	if src.LastSeenAt.Valid {
		seen = src.LastSeenAt.String
	}
	if src.AgentVersion.Valid {
		ver = src.AgentVersion.String
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"sourceId": src.ID, "lastSeenAt": seen, "lastMetricsSeq": src.LastMetricsSeq, "agentVersion": ver})
}
