package server

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/backup"
)

// sessionToken returns the login cookie the env's browser currently holds.
func (e *env) sessionToken() string {
	e.t.Helper()
	u, err := url.Parse(e.ts.URL)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, ck := range e.c.Jar.Cookies(u) {
		if ck.Name == auth.CookieName && ck.Value != "" {
			return ck.Value
		}
	}
	e.t.Fatal("no session cookie")
	return ""
}

// Logout must drop the previews issued to that login session, and only those. A preview holds a
// decrypted backup payload in memory; leaving it behind keeps plaintext alive after the session is
// gone. Another session's preview stays bound to its own cookie and must survive.
func TestLogoutDropsOnlyTheSessionPreviews(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}
	env, payload, err := e.srv.Backups.OpenUpload(raw, "")
	if err != nil {
		t.Fatalf("open upload: %v", err)
	}
	token := e.sessionToken()
	own, err := e.srv.Backups.Previews.Put(token, env, payload)
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.srv.Backups.Previews.Put("another-session", env, payload)
	if err != nil {
		t.Fatal(err)
	}

	if status, body := e.postJSON(e.c, "/api/auth/logout", nil, nil); status != http.StatusOK {
		t.Fatalf("logout %d: %s", status, truncate(body))
	}
	if got := e.srv.Backups.Previews.Len(); got != 1 {
		t.Fatalf("previews after logout = %d, want 1 (only the logged-out session's preview is dropped)", got)
	}
	if _, err := e.srv.Backups.Previews.Take(own.ID, token); !errors.Is(err, backup.ErrPreviewInvalid) {
		t.Fatalf("logged-out session's preview err = %v, want ErrPreviewInvalid", err)
	}
	if _, err := e.srv.Backups.Previews.Take(other.ID, "another-session"); err != nil {
		t.Fatalf("another session's preview was dropped by logout: %v", err)
	}
}

// A successful restore replaces every session, so every preview bound to those sessions must be
// cleared, including previews other than the one that was applied. The applied preview is already
// consumed by Take; the rest would otherwise keep plaintext payloads for revoked sessions.
func TestRestoreClearsRemainingPreviews(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}
	applied := e.previewOne(e.c, raw, "")
	other := e.previewOne(e.c, raw, "")
	if got := e.srv.Backups.Previews.Len(); got != 2 {
		t.Fatalf("previews before restore = %d, want 2", got)
	}

	status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": applied.PreviewID}, nil)
	if status != http.StatusOK {
		t.Fatalf("apply %d: %s", status, truncate(body))
	}
	if got := e.srv.Backups.Previews.Len(); got != 0 {
		t.Fatalf("previews after restore = %d, want 0 (previews must not survive revoked sessions)", got)
	}
	if _, err := e.srv.Backups.Previews.Take(other.PreviewID, "another-session"); !errors.Is(err, backup.ErrPreviewInvalid) {
		t.Fatalf("surviving preview err = %v, want ErrPreviewInvalid", err)
	}
}
