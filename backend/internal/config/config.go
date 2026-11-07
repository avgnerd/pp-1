// Package config loads data/config.json, creating it with defaults when it is missing.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Config is the on-disk server configuration. Runtime-editable options live in the settings table.
type Config struct {
	Listen         string   `json:"listen"`
	PublicURL      string   `json:"public_url"`
	TrustedProxies []string `json:"trusted_proxies"`
	FFmpegPath     string   `json:"ffmpeg_path"`
	FFprobePath    string   `json:"ffprobe_path"`
	// SecretKey is a 64 hex char (32 byte) key. Empty means "use data/secret.key, create if missing".
	SecretKey string `json:"secret_key,omitempty"`

	// Dropbox endpoints are overridable so tests can point at a fake server.
	DropboxAPIBase     string `json:"dropbox_api_base,omitempty"`
	DropboxContentBase string `json:"dropbox_content_base,omitempty"`
	DropboxAuthorize   string `json:"dropbox_authorize_url,omitempty"`
	DropboxTokenURL    string `json:"dropbox_token_url,omitempty"`

	// Populated at load time, not persisted.
	DataDir string `json:"-"`
	Path    string `json:"-"`
	Key     []byte `json:"-"`
}

const (
	DefaultAPIBase     = "https://api.dropboxapi.com"
	DefaultContentBase = "https://content.dropboxapi.com"
	DefaultAuthorize   = "https://www.dropbox.com/oauth2/authorize"
	DefaultTokenURL    = "https://api.dropboxapi.com/oauth2/token"
)

func defaults() *Config {
	return &Config{
		Listen:         ":8080",
		PublicURL:      "",
		TrustedProxies: []string{},
	}
}

// Load reads <dataDir>/config.json. When the file does not exist it is created with defaults.
// Environment variables DVFS_LISTEN and DVFS_PUBLIC_URL override file values.
func Load(dataDir string) (*Config, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(abs, "config.json")
	cfg := defaults()
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := write(path, cfg); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	cfg.DataDir = abs
	cfg.Path = path

	if v := os.Getenv("DVFS_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("DVFS_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := os.Getenv("DVFS_FFMPEG"); v != "" {
		cfg.FFmpegPath = v
	}
	if v := os.Getenv("DVFS_FFPROBE"); v != "" {
		cfg.FFprobePath = v
	}
	// Test/demo hook: point every Dropbox endpoint at one fake server.
	if v := strings.TrimRight(os.Getenv("DVFS_DROPBOX_BASE"), "/"); v != "" {
		cfg.DropboxAPIBase, cfg.DropboxContentBase = v, v
		cfg.DropboxAuthorize, cfg.DropboxTokenURL = v+"/oauth2/authorize", v+"/oauth2/token"
	}
	if cfg.DropboxAPIBase == "" {
		cfg.DropboxAPIBase = DefaultAPIBase
	}
	if cfg.DropboxContentBase == "" {
		cfg.DropboxContentBase = DefaultContentBase
	}
	if cfg.DropboxAuthorize == "" {
		cfg.DropboxAuthorize = DefaultAuthorize
	}
	if cfg.DropboxTokenURL == "" {
		cfg.DropboxTokenURL = DefaultTokenURL
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("public_url must be an absolute http(s) URL, got %q", cfg.PublicURL)
		}
	}
	cfg.FFmpegPath = resolveTool(cfg.FFmpegPath, "ffmpeg")
	cfg.FFprobePath = resolveTool(cfg.FFprobePath, "ffprobe")

	key, err := loadKey(cfg)
	if err != nil {
		return nil, err
	}
	cfg.Key = key
	return cfg, nil
}

func write(path string, cfg *Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func loadKey(cfg *Config) ([]byte, error) {
	if cfg.SecretKey != "" {
		k, err := hex.DecodeString(cfg.SecretKey)
		if err != nil || len(k) != 32 {
			return nil, errors.New("secret_key must be 64 hex characters")
		}
		return k, nil
	}
	if v := os.Getenv("DVFS_SECRET_KEY"); v != "" {
		k, err := hex.DecodeString(v)
		if err != nil || len(k) != 32 {
			return nil, errors.New("DVFS_SECRET_KEY must be 64 hex characters")
		}
		return k, nil
	}
	p := filepath.Join(cfg.DataDir, "secret.key")
	b, err := os.ReadFile(p)
	if err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("%s is corrupt: expected 64 hex characters", p)
		}
		return k, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, []byte(hex.EncodeToString(k)), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// resolveTool returns an explicit path if set, else tools/ffmpeg/bin next to the executable
// or working directory, else the PATH lookup. Empty result means the tool is unavailable.
func resolveTool(explicit, name string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		if p, err := exec.LookPath(explicit); err == nil {
			return p
		}
		return ""
	}
	exe := name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	var roots []string
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd, filepath.Dir(wd))
	}
	if ep, err := os.Executable(); err == nil {
		d := filepath.Dir(ep)
		roots = append(roots, d, filepath.Dir(d))
	}
	for _, r := range roots {
		p := filepath.Join(r, "tools", "ffmpeg", "bin", exe)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// Port returns the TCP port of the listen address (default 8080).
func (c *Config) Port() string {
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil || port == "" {
		return "8080"
	}
	return port
}

// LocalhostCallback is the redirect URI used by the "localhost" OAuth mode.
func (c *Config) LocalhostCallback() string {
	return "http://localhost:" + c.Port() + "/api/oauth/callback"
}

// PublicCallback is the redirect URI used by the "public_url" OAuth mode, or "" when unset.
func (c *Config) PublicCallback() string {
	if c.PublicURL == "" {
		return ""
	}
	return c.PublicURL + "/api/oauth/callback"
}

// SecureCookies reports whether cookies should carry the Secure flag.
func (c *Config) SecureCookies() bool {
	return strings.HasPrefix(c.PublicURL, "https://")
}
