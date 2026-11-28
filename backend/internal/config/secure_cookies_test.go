package config

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureCookiesUsesParsedScheme(t *testing.T) {
	cases := []struct {
		name      string
		publicURL string
		want      bool
	}{
		{"https", "https://example.com", true},
		{"uppercase_https", "HTTPS://example.com", true},
		{"mixed_case_https", "HtTpS://example.com", true},
		{"http", "http://example.com", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{PublicURL: tc.publicURL}
			if got := cfg.SecureCookies(); got != tc.want {
				t.Fatalf("SecureCookies() = %v, want %v for public_url %q", got, tc.want, tc.publicURL)
			}
			req := httptest.NewRequest("GET", "http://internal.example/", nil)
			if got := cfg.SecureRequest(req); got != tc.want {
				t.Fatalf("SecureRequest() = %v, want %v for public_url %q", got, tc.want, tc.publicURL)
			}
		})
	}
}

func TestLoadAcceptsUppercaseHTTPSPublicURL(t *testing.T) {
	t.Setenv("DVFS_PUBLIC_URL", "")
	dir := t.TempDir()
	writeConfig(t, dir, `{"public_url": "HTTPS://example.com"}`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load rejected an uppercase https public_url: %v", err)
	}
	if !cfg.SecureCookies() {
		t.Fatal("SecureCookies() = false for an uppercase https public_url")
	}
}

func TestLoadRejectsIncompletePublicURL(t *testing.T) {
	t.Setenv("DVFS_PUBLIC_URL", "")
	dir := t.TempDir()
	writeConfig(t, dir, `{"public_url": "https://"}`)
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted public_url \"https://\" with no host")
	}
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
