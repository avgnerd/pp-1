package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/backup"
	"github.com/dvfs/dropbox-vfs-manager/internal/media"
	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// ---- helpers ----

// newClient returns another browser against the same test server (its own cookie jar).
func (e *env) newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// loginAs signs in with the given credentials.
func (e *env) loginAs(c *http.Client, username, password string) int {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", auth.CSRFValue)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// getAs performs an authenticated GET and returns the status and body.
func (e *env) getAs(c *http.Client, path string) (int, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// postRaw sends a raw body with the given content type.
func (e *env) postRaw(c *http.Client, path, contentType string, body []byte) (int, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Requested-With", auth.CSRFValue)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// postJSON sends a JSON value and decodes a 200 response into out.
func (e *env) postJSON(c *http.Client, path string, in any, out any) (int, []byte) {
	e.t.Helper()
	body, _ := json.Marshal(in)
	status, respBody := e.postRaw(c, path, "application/json", body)
	if out != nil && status == http.StatusOK {
		if err := json.Unmarshal(respBody, out); err != nil {
			e.t.Fatalf("%s: decode response: %v (%s)", path, err, truncate(respBody))
		}
	}
	return status, respBody
}

// postMultipart uploads a backup file the way the admin UI does.
func (e *env) postMultipart(c *http.Client, path string, file []byte, password string) (int, []byte) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "dvfs-backup.json")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := fw.Write(file); err != nil {
		e.t.Fatal(err)
	}
	if password != "" {
		if err := mw.WriteField("password", password); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		e.t.Fatal(err)
	}
	return e.postRaw(c, path, mw.FormDataContentType(), buf.Bytes())
}

// exportBackup asks for a backup file and returns the envelope, the raw bytes and the response.
func (e *env) exportBackup(c *http.Client, in map[string]any) (*backup.Envelope, []byte, *http.Response) {
	e.t.Helper()
	body, _ := json.Marshal(in)
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/admin/backup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", auth.CSRFValue)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env backup.Envelope
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &env); err != nil {
			e.t.Fatalf("backup envelope: %v (%s)", err, truncate(raw))
		}
	}
	return &env, raw, resp
}

// previewOne uploads a backup file and returns its preview.
func (e *env) previewOne(c *http.Client, file []byte, password string) *backup.RestorePreview {
	e.t.Helper()
	status, body := e.postMultipart(c, "/api/admin/restore/preview", file, password)
	if status != http.StatusOK {
		e.t.Fatalf("preview status %d: %s", status, truncate(body))
	}
	var preview backup.RestorePreview
	if err := json.Unmarshal(body, &preview); err != nil {
		e.t.Fatalf("preview body: %v (%s)", err, truncate(body))
	}
	return &preview
}

// previewAndApply uploads a backup file, previews it and applies it.
func (e *env) previewAndApply(c *http.Client, file []byte, password string) (*backup.RestorePreview, map[string]any) {
	e.t.Helper()
	preview := e.previewOne(c, file, password)
	var applied map[string]any
	if status, body := e.postJSON(c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, &applied); status != http.StatusOK {
		e.t.Fatalf("apply status %d: %s", status, truncate(body))
	}
	return preview, applied
}

// setSetting changes one setting through the API.
func (e *env) setSetting(key string, value any) {
	e.t.Helper()
	status, body := e.getAs(e.c, "/api/admin/settings")
	if status != http.StatusOK {
		e.t.Fatalf("get settings %d: %s", status, truncate(body))
	}
	var current map[string]any
	if err := json.Unmarshal(body, &current); err != nil {
		e.t.Fatal(err)
	}
	current[key] = value
	raw, _ := json.Marshal(current)
	req, _ := http.NewRequest("PUT", e.ts.URL+"/api/admin/settings", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", auth.CSRFValue)
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("put settings %d: %s", resp.StatusCode, truncate(respBody))
	}
}

// connectNamed creates a connected storage and gives it a display name: a backup payload must
// carry one for the restore validation to accept it.
func (e *env) connectNamed(appKey, name, mount string) int64 {
	e.t.Helper()
	id := e.connect(appKey, mount)
	body, _ := json.Marshal(map[string]any{
		"name": name, "mount_path": mount, "root_path": "/", "cache_ttl_min": 30, "app_key": appKey,
	})
	req, _ := http.NewRequest("PUT", e.ts.URL+"/api/admin/storages/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", auth.CSRFValue)
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("rename storage %d: %s", resp.StatusCode, truncate(raw))
	}
	return id
}

// mediaSource is a transcode request used to prove the manager is still usable.
func mediaSource() media.Source {
	return media.Source{
		Key: "test", Client: "test", Duration: 30, Mode: "remux",
		URL: func(context.Context) (string, error) { return "http://example.invalid/x.mp4", nil },
	}
}

// countRows runs a COUNT(*) query.
func countRows(t *testing.T, d *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// truncate keeps failure messages readable when a response body is huge.
func truncate(b []byte) string {
	if len(b) > 400 {
		return string(b[:400]) + "..."
	}
	return string(b)
}

// ---- 1. export, preview, apply ----

func TestBackupExportHeadersAndRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connectNamed("app-key-1", "Main", "/main")

	const pw = "correct horse battery"
	env, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": true, "password": pw})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, truncate(raw))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	created, err := time.Parse(time.RFC3339, env.CreatedAt)
	if err != nil {
		t.Fatalf("created_at %q: %v", env.CreatedAt, err)
	}
	wantName := `attachment; filename="dvfs-backup-` + created.UTC().Format("20060102-150405") + `.json"`
	if cd := resp.Header.Get("Content-Disposition"); cd != wantName {
		t.Fatalf("content-disposition = %q, want %q", cd, wantName)
	}
	if !env.Encrypted || env.EncryptionMode != backup.ModePassword || env.Ciphertext == "" || env.Payload != nil {
		t.Fatalf("envelope = %+v", env)
	}
	for _, secretValue := range []string{"app-key-1", "password_hash", "refresh"} {
		if strings.Contains(string(raw), secretValue) {
			t.Fatalf("backup body contains %q", secretValue)
		}
	}

	// The preview reports what would change, never a secret.
	status, body := e.postMultipart(e.c, "/api/admin/restore/preview", raw, pw)
	if status != http.StatusOK {
		t.Fatalf("preview %d: %s", status, truncate(body))
	}
	var preview backup.RestorePreview
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatalf("preview body: %v (%s)", err, truncate(body))
	}
	if preview.FormatVersion != 1 || !preview.Encrypted || preview.EncryptionMode != backup.ModePassword {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.AdminUsername != "admin" {
		t.Fatalf("admin username = %q", preview.AdminUsername)
	}
	if want := countRows(t, e.srv.DB, `SELECT COUNT(*) FROM settings`); preview.SettingsCount != want {
		t.Fatalf("settings_count = %d, want %d", preview.SettingsCount, want)
	}
	if len(preview.Storages) != 1 || preview.Storages[0].MountPath != "/main" || preview.Storages[0].Name != "Main" {
		t.Fatalf("preview storages = %+v", preview.Storages)
	}
	if len(preview.PreviewID) != 32 {
		t.Fatalf("preview id = %q", preview.PreviewID)
	}
	for _, forbidden := range []string{"password_hash", "app_secret", "refresh_token", "app_key"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("preview leaks %q: %s", forbidden, truncate(body))
		}
	}

	// Apply, then the old session is gone and the restored admin can log in again.
	var applied struct {
		OK             bool     `json:"ok"`
		FailedStorages []string `json:"failed_storages"`
	}
	if status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, &applied); status != http.StatusOK {
		t.Fatalf("apply %d: %s", status, truncate(body))
	}
	if !applied.OK || applied.FailedStorages == nil || len(applied.FailedStorages) != 0 {
		t.Fatalf("apply response = %+v", applied)
	}
	if status, _ := e.getAs(e.c, "/api/admin/settings"); status != http.StatusUnauthorized {
		t.Fatalf("old session still valid: %d", status)
	}
	if status := e.loginAs(e.c, "admin", auth.DefaultPassword); status != http.StatusOK {
		t.Fatalf("restored admin cannot log in: %d", status)
	}
	if status, body := e.getAs(e.c, "/api/admin/storages"); status != http.StatusOK || !strings.Contains(string(body), "/main") {
		t.Fatalf("restored storages %d: %s", status, truncate(body))
	}
}

func TestBackupUnencryptedRequiresConfirmation(t *testing.T) {
	e := newEnv(t)
	e.login()
	if status, _ := e.postJSON(e.c, "/api/admin/backup", map[string]any{"encrypted": false}, nil); status != http.StatusBadRequest {
		t.Fatalf("unconfirmed plain backup status = %d", status)
	}
	env, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, truncate(raw))
	}
	if env.Encrypted || env.EncryptionMode != backup.ModeNone || env.Payload == nil {
		t.Fatalf("plain envelope = %+v", env)
	}
	if env.KDF != nil || env.Nonce != "" || env.Ciphertext != "" {
		t.Fatal("plain envelope carries encryption fields")
	}
	// It can be previewed without a password.
	if status, body := e.postMultipart(e.c, "/api/admin/restore/preview", raw, ""); status != http.StatusOK {
		t.Fatalf("preview %d: %s", status, truncate(body))
	}
}

func TestBackupPasswordRules(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"seven runes", map[string]any{"encrypted": true, "password": "1234567"}, http.StatusBadRequest},
		{"missing password", map[string]any{"encrypted": true}, http.StatusBadRequest},
		{"too long", map[string]any{"encrypted": true, "password": strings.Repeat("a", 129)}, http.StatusBadRequest},
		{"eight runes", map[string]any{"encrypted": true, "password": "12345678"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, raw, resp := e.exportBackup(e.c, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.want, truncate(raw))
			}
		})
	}
}

// ---- 2. preview validation ----

func TestRestorePreviewRejectsInstanceKeyFile(t *testing.T) {
	e := newEnv(t)
	e.login()
	name, err := e.srv.Backups.CreateSnapshot(e.t.Context())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(e.srv.Backups.SnapshotDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	status, body := e.postMultipart(e.c, "/api/admin/restore/preview", raw, "")
	if status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, truncate(body))
	}
	if !strings.Contains(string(body), "instance-key") {
		t.Fatalf("message does not explain the mode: %s", truncate(body))
	}
}

func TestRestorePreviewRejectsInvalidPayload(t *testing.T) {
	e := newEnv(t)
	e.login()
	// Store the settings rows first so the exported file has something to break.
	e.setSetting("per_page", 30)
	env, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}
	found := false
	for i := range env.Payload.Settings {
		if env.Payload.Settings[i].Key == "per_page" {
			env.Payload.Settings[i].Value = "1"
			found = true
		}
	}
	if !found {
		t.Fatalf("backup has no per_page setting: %+v", env.Payload.Settings)
	}
	broken, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	status, body := e.postMultipart(e.c, "/api/admin/restore/preview", broken, "")
	if status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", status, truncate(body))
	}
	if !strings.Contains(string(body), "per_page") {
		t.Fatalf("message does not name the setting: %s", truncate(body))
	}
	// Nothing was written.
	if got := e.srv.Settings.Get().PerPage; got != 30 {
		t.Fatalf("rejected preview changed the live settings: per_page = %d", got)
	}
}

// TestBcryptBackupRestoresAndLogsIn is the end to end compatibility proof for a backup written by a
// build that hashed the admin password with bcrypt: the restore screen accepts the file, the apply
// stores it, and the restored instance logs in with that password while rejecting a wrong one.
func TestBcryptBackupRestoresAndLogsIn(t *testing.T) {
	e := newEnv(t)
	e.login()
	const password = "bcrypt-restored-password"
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	env, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}
	env.Payload.Admin.PasswordHash = string(h)
	file, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	preview, applied := e.previewAndApply(e.c, file, "")
	if preview.AdminUsername != "admin" {
		t.Fatalf("preview admin username = %q", preview.AdminUsername)
	}
	if done, _ := applied["ok"].(bool); !done {
		t.Fatalf("apply result: %v", applied)
	}

	if status := e.loginAs(e.newClient(), "admin", password); status != http.StatusOK {
		t.Fatalf("login with the restored bcrypt password: %d", status)
	}
	if status := e.loginAs(e.newClient(), "admin", "wrong-password"); status != http.StatusUnauthorized {
		t.Fatalf("login with a wrong password: %d, want 401", status)
	}
}

// ---- 3. the 10 MiB boundary ----

// paddedBackup returns the envelope JSON with an unknown top-level field added so the file is
// exactly size bytes and is still a valid backup.
func paddedBackup(t *testing.T, env *backup.Envelope, size int) []byte {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2 || raw[len(raw)-1] != '}' {
		t.Fatalf("unexpected envelope JSON: %s", truncate(raw))
	}
	const openField = `,"padding":"`
	const closeField = `"}`
	n := size - (len(raw) - 1) - len(openField) - len(closeField)
	if n < 0 {
		t.Fatalf("envelope is already %d bytes, cannot pad to %d", len(raw), size)
	}
	out := make([]byte, 0, size)
	out = append(out, raw[:len(raw)-1]...)
	out = append(out, openField...)
	out = append(out, bytes.Repeat([]byte("a"), n)...)
	out = append(out, closeField...)
	if len(out) != size {
		t.Fatalf("padded size = %d, want %d", len(out), size)
	}
	return out
}

func TestRestorePreviewSizeLimit(t *testing.T) {
	e := newEnv(t)
	e.login()
	env, _, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d", resp.StatusCode)
	}
	exact := paddedBackup(t, env, maxBackupBytes)
	over := paddedBackup(t, env, maxBackupBytes+1)

	t.Run("json body at the limit", func(t *testing.T) {
		status, body := e.postRaw(e.c, "/api/admin/restore/preview", "application/json", exact)
		if status == http.StatusRequestEntityTooLarge {
			t.Fatalf("a %d byte file was rejected as too large: %s", len(exact), truncate(body))
		}
		if status != http.StatusOK {
			t.Fatalf("status %d: %s", status, truncate(body))
		}
	})
	t.Run("json body one byte over", func(t *testing.T) {
		status, body := e.postRaw(e.c, "/api/admin/restore/preview", "application/json", over)
		if status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413: %s", status, truncate(body))
		}
	})
	t.Run("multipart at the limit", func(t *testing.T) {
		status, body := e.postMultipart(e.c, "/api/admin/restore/preview", exact, "")
		if status == http.StatusRequestEntityTooLarge {
			t.Fatalf("a %d byte upload was rejected as too large: %s", len(exact), truncate(body))
		}
		if status != http.StatusOK {
			t.Fatalf("status %d: %s", status, truncate(body))
		}
	})
	t.Run("multipart one byte over", func(t *testing.T) {
		status, body := e.postMultipart(e.c, "/api/admin/restore/preview", over, "")
		if status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413: %s", status, truncate(body))
		}
	})
	t.Run("json wrapper over the limit", func(t *testing.T) {
		wrapper, err := json.Marshal(map[string]any{"backup": json.RawMessage(over), "password": ""})
		if err != nil {
			t.Fatal(err)
		}
		status, body := e.postRaw(e.c, "/api/admin/restore/preview", "application/json", wrapper)
		if status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413: %s", status, truncate(body))
		}
	})
	t.Run("json wrapper at the limit", func(t *testing.T) {
		wrapper, err := json.Marshal(map[string]any{"backup": json.RawMessage(exact), "password": ""})
		if err != nil {
			t.Fatal(err)
		}
		status, body := e.postRaw(e.c, "/api/admin/restore/preview", "application/json", wrapper)
		if status != http.StatusOK {
			t.Fatalf("status %d: %s", status, truncate(body))
		}
	})
}

// ---- 4. apply guards ----

func TestRestoreApplyRejectsUnknownPreview(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, id := range []string{"", "0123456789abcdef0123456789abcdef"} {
		status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": id}, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("id %q status %d: %s", id, status, truncate(body))
		}
	}
	if got := e.srv.Backups.Previews.Len(); got != 0 {
		t.Fatalf("previews = %d", got)
	}
}

func TestRestoreApplyPreviewIsSingleUse(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, raw, _ := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	preview := e.previewOne(e.c, raw, "")
	if status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil); status != http.StatusOK {
		t.Fatalf("first apply %d: %s", status, truncate(body))
	}
	// The session is gone now, so a replay must be refused even after logging in again.
	e.login()
	if status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil); status != http.StatusBadRequest {
		t.Fatalf("replay status %d: %s", status, truncate(body))
	}
}

func TestRestoreApplyIsBoundToTheIssuingSession(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, raw, _ := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	preview := e.previewOne(e.c, raw, "")

	other := e.newClient()
	if status := e.loginAs(other, "admin", auth.DefaultPassword); status != http.StatusOK {
		t.Fatalf("second login %d", status)
	}
	if status, body := e.postJSON(other, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil); status != http.StatusBadRequest {
		t.Fatalf("other session applied the preview: %d %s", status, truncate(body))
	}
	// The rightful session can still apply it.
	if status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil); status != http.StatusOK {
		t.Fatalf("owner apply %d: %s", status, truncate(body))
	}
}

func TestRestoreApplyConflictsWhenOneIsRunning(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, raw, _ := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	preview := e.previewOne(e.c, raw, "")

	// Hold the restore critical section so a second apply has to be refused.
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.srv.restoreHook = func() {
		once.Do(func() { close(started) })
		<-release
	}
	defer func() { e.srv.restoreHook = nil }()

	done := make(chan int, 1)
	go func() {
		status, _ := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil)
		done <- status
	}()
	<-started

	status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil)
	if status != http.StatusConflict {
		t.Fatalf("concurrent apply status %d: %s", status, truncate(body))
	}
	var conflict struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Code != "restore_in_progress" || conflict.Error == "" {
		t.Fatalf("conflict body = %+v", conflict)
	}
	close(release)
	if status := <-done; status != http.StatusOK {
		t.Fatalf("first apply finished with %d", status)
	}
}

// TestStorageMutationsRejectedWhileRestoreRuns requires a storage mutation that arrives during a
// restore to fail fast instead of writing a row the restore transaction or runtime refresh can
// overwrite.
func TestStorageMutationsRejectedWhileRestoreRuns(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, raw, _ := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	preview := e.previewOne(e.c, raw, "")

	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	e.srv.restoreHook = func() {
		startOnce.Do(func() { close(started) })
		<-release
	}
	defer func() { e.srv.restoreHook = nil }()
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	done := make(chan int, 1)
	go func() {
		status, _ := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil)
		done <- status
	}()
	<-started

	status, body := e.postJSON(e.c, "/api/admin/storages", map[string]any{
		"name": "intruder", "mount_path": "/intruder", "app_key": "k", "app_secret": "s",
	}, nil)
	unblock()
	applyStatus := <-done
	if status != http.StatusConflict {
		t.Fatalf("storage create during restore = %d, want 409: %s", status, truncate(body))
	}
	if applyStatus != http.StatusOK {
		t.Fatalf("restore finished with %d", applyStatus)
	}
}

// TestRestoreGateReleasedAfterFailure requires a restore that never reaches the transaction to
// hand the storage mutation gate back, so the admin can keep editing storages.
func TestRestoreGateReleasedAfterFailure(t *testing.T) {
	e := newEnv(t)
	e.login()
	status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": "unknown"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("unknown preview apply = %d: %s", status, truncate(body))
	}
	status, body = e.postJSON(e.c, "/api/admin/storages", map[string]any{
		"name": "after-failure", "mount_path": "/after-failure", "app_key": "k", "app_secret": "s",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("storage create after failed restore = %d: %s", status, truncate(body))
	}
}

// TestRestoreAuthLockReleasedAfterApplyFailure makes the restore transaction fail after the
// credential lock is taken. The lock and the storage gate must both be released, so a login and a
// storage edit still work afterwards.
func TestRestoreAuthLockReleasedAfterApplyFailure(t *testing.T) {
	e := newEnv(t)
	e.login()
	_, raw, _ := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	preview := e.previewOne(e.c, raw, "")
	if _, err := e.srv.DB.Exec(`CREATE TRIGGER fail_restore_admin BEFORE INSERT ON admin BEGIN SELECT RAISE(ABORT, 'restore failed'); END`); err != nil {
		t.Fatal(err)
	}
	status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil)
	if status == http.StatusOK {
		t.Fatalf("restore reported success although the transaction failed: %s", truncate(body))
	}
	if _, err := e.srv.DB.Exec(`DROP TRIGGER fail_restore_admin`); err != nil {
		t.Fatal(err)
	}

	loginClient := e.newClient()
	loginClient.Timeout = 5 * time.Second
	if st := e.loginAs(loginClient, "admin", auth.DefaultPassword); st != http.StatusOK {
		t.Fatalf("login after failed restore = %d", st)
	}
	status, body = e.postJSON(e.c, "/api/admin/storages", map[string]any{
		"name": "after-rollback", "mount_path": "/after-rollback", "app_key": "k", "app_secret": "s",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("storage create after rolled-back restore = %d: %s", status, truncate(body))
	}
}

// ---- 5. runtime refresh ----

func TestRestoreRefreshesRuntimeState(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connectNamed("app-key-1", "Main", "/main")
	e.setSetting("per_page", 55)

	_, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": true, "password": "correct horse battery"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}

	// Move the live state away from the backup and plant data a restore must keep.
	e.setSetting("per_page", 77)
	thumbsDir := filepath.Join(e.srv.Cfg.DataDir, "thumbs")
	hlsDir := filepath.Join(e.srv.Cfg.DataDir, "hls")
	for _, dir := range []string{thumbsDir, hlsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	thumbFile := filepath.Join(thumbsDir, "cached-thumb.jpg")
	hlsFile := filepath.Join(hlsDir, "seg0.m4s")
	for _, path := range []string{thumbFile, hlsFile} {
		if err := os.WriteFile(path, []byte("cache"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.srv.DB.Exec(`INSERT INTO media_probe(file_id, rev, data, created_at) VALUES('f','r','{}',1)`); err != nil {
		t.Fatal(err)
	}
	e.srv.Cache.Set("dir:/main:/", "cached", 6, time.Minute)

	var listenerCalls int
	e.srv.Settings.OnChange(func(old, next settings.Settings) { listenerCalls++ })

	preview, applied := e.previewAndApply(e.c, raw, "correct horse battery")
	if len(preview.PreviewID) != 32 {
		t.Fatalf("preview id = %q", preview.PreviewID)
	}
	if failed, _ := applied["failed_storages"].([]any); len(failed) != 0 {
		t.Fatalf("failed storages = %v", failed)
	}

	// Settings store, in memory and in the database.
	if got := e.srv.Settings.Get().PerPage; got != 55 {
		t.Fatalf("in-memory per_page = %d, want 55", got)
	}
	if n := countRows(t, e.srv.DB, `SELECT COUNT(*) FROM settings WHERE key='per_page' AND value='55'`); n != 1 {
		t.Fatalf("database per_page was not restored (%d rows)", n)
	}
	if listenerCalls == 0 {
		t.Fatal("settings change listeners were not called")
	}
	// Caches.
	if _, ok := e.srv.Cache.Get("dir:/main:/"); ok {
		t.Fatal("cache entry survived the restore")
	}
	// HLS sessions stopped, cached files kept.
	if n := e.srv.HLS.Count(); n != 0 {
		t.Fatalf("HLS sessions = %d, want 0", n)
	}
	for _, path := range []string{thumbFile, hlsFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was deleted by the restore: %v", path, err)
		}
	}
	// Media probe rows kept.
	if n := countRows(t, e.srv.DB, `SELECT COUNT(*) FROM media_probe`); n != 1 {
		t.Fatalf("media_probe rows = %d, want 1", n)
	}
	// Storage clients rebuilt from the restored list.
	storages := e.srv.St.All()
	if len(storages) != 1 || storages[0].Name != "Main" {
		t.Fatalf("storages = %+v", storages)
	}
	if _, err := e.srv.St.Client(storages[0].ID); err != nil {
		t.Fatalf("storage client not rebuilt: %v", err)
	}
	// Login sessions revoked, then a fresh login works.
	if status, _ := e.getAs(e.c, "/api/admin/settings"); status != http.StatusUnauthorized {
		t.Fatalf("pre-restore cookie still accepted: %d", status)
	}
	if status := e.loginAs(e.c, "admin", auth.DefaultPassword); status != http.StatusOK {
		t.Fatalf("login after restore: %d", status)
	}
	// The pre-restore snapshot holds the state that was replaced (per_page 77).
	list, err := e.srv.Backups.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(list))
	}
	_, snapPayload, err := e.srv.Backups.OpenSnapshotFile(list[0].ID)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	var snapPerPage string
	for _, rec := range snapPayload.Settings {
		if rec.Key == "per_page" {
			snapPerPage = rec.Value
		}
	}
	if snapPerPage != "77" {
		t.Fatalf("snapshot per_page = %q, want the pre-restore value 77", snapPerPage)
	}
}

func TestStopSessionsKeepsManagerUsable(t *testing.T) {
	e := newEnv(t)
	e.srv.HLS.StopSessions()
	if n := e.srv.HLS.Count(); n != 0 {
		t.Fatalf("sessions = %d", n)
	}
	// The manager (and its reaper) is still alive: opening fails because ffmpeg is missing, but
	// it must not panic on a closed manager.
	if _, err := e.srv.HLS.Open(mediaSource()); err == nil {
		t.Fatal("open without ffmpeg unexpectedly succeeded")
	}
}

// ---- 6. cross-instance restore ----

func TestRestoreIntoAnotherInstance(t *testing.T) {
	source := newEnv(t)
	source.login()
	source.connectNamed("app-key-source", "Source", "/source")
	source.setSetting("per_page", 99)
	_, raw, resp := source.exportBackup(source.c, map[string]any{"encrypted": true, "password": "correct horse battery"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}

	target := newEnv(t)
	target.login()
	target.connectNamed("app-key-target", "Target", "/target")
	if bytes.Equal(source.srv.Cfg.Key, target.srv.Cfg.Key) {
		t.Fatal("the two test instances share a master key")
	}

	target.previewAndApply(target.c, raw, "correct horse battery")

	storages := target.srv.St.All()
	if len(storages) != 1 || storages[0].MountPath != "/source" {
		t.Fatalf("target storages = %+v", storages)
	}
	if got := target.srv.Settings.Get().PerPage; got != 99 {
		t.Fatalf("target per_page = %d", got)
	}
	// The credentials now carry the target instance key, so its client can read them.
	if _, err := target.srv.St.Client(storages[0].ID); err != nil {
		t.Fatalf("target cannot use the restored storage: %v", err)
	}
	var enc []byte
	if err := target.srv.DB.QueryRow(`SELECT refresh_token_enc FROM storages WHERE id = ?`, storages[0].ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	plain, err := target.srv.Box.Open(enc)
	if err != nil {
		t.Fatalf("target box cannot open the restored token: %v", err)
	}
	if len(plain) == 0 {
		t.Fatal("restored refresh token is empty")
	}
	if _, err := source.srv.Box.Open(enc); err == nil {
		t.Fatal("restored token is still readable with the source instance key")
	}
}

// ---- 7. pre-restore snapshots ----

func TestSnapshotEndpoints(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connectNamed("app-key-1", "Main", "/main")
	_, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}
	// Applying the backup stores the first pre-restore snapshot.
	e.previewAndApply(e.c, raw, "")
	e.login()

	status, body := e.getAs(e.c, "/api/admin/backups/snapshots")
	if status != http.StatusOK {
		t.Fatalf("list %d: %s", status, truncate(body))
	}
	var list struct {
		Snapshots []backup.SnapshotInfo `json:"snapshots"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Snapshots) != 1 {
		t.Fatalf("snapshots = %+v", list.Snapshots)
	}
	snap := list.Snapshots[0]
	if !backup.ValidSnapshotID(snap.ID) || snap.Size == 0 || snap.AppVersion == "" || snap.StorageCount != 1 {
		t.Fatalf("snapshot info = %+v", snap)
	}
	if _, err := time.Parse(time.RFC3339, snap.CreatedAt); err != nil {
		t.Fatalf("created_at %q: %v", snap.CreatedAt, err)
	}

	// Preview the snapshot (no password) and restore from it.
	status, body = e.postRaw(e.c, "/api/admin/backups/snapshots/"+snap.ID+"/preview", "application/json", nil)
	if status != http.StatusOK {
		t.Fatalf("snapshot preview %d: %s", status, truncate(body))
	}
	var preview backup.RestorePreview
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.EncryptionMode != backup.ModeInstanceKey || preview.AdminUsername != "admin" {
		t.Fatalf("preview = %+v", preview)
	}
	if status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil); status != http.StatusOK {
		t.Fatalf("apply snapshot %d: %s", status, truncate(body))
	}
	e.login()

	// The second restore stored another snapshot; both are listed newest first.
	status, body = e.getAs(e.c, "/api/admin/backups/snapshots")
	if status != http.StatusOK {
		t.Fatalf("list %d: %s", status, truncate(body))
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Snapshots) != 2 {
		t.Fatalf("snapshots after second restore = %d", len(list.Snapshots))
	}
	if list.Snapshots[0].ID == list.Snapshots[1].ID {
		t.Fatal("two snapshots share a name")
	}
}

func TestSnapshotPreviewRejectsBadNames(t *testing.T) {
	e := newEnv(t)
	e.login()
	paths := []string{
		"pre-restore-x.json",              // well formed but missing
		"pre-restore-20261101-030000.txt", // wrong extension
		"pre-restore-.json",               // empty middle
		"pre-restore-..json",              // dot dot
		"pre-restore-..a.json",            // dot dot inside
		`pre-restore-a\b.json`,            // backslash
		"pre-restore-a%2F..%2Fb.json",     // encoded traversal
		"..%2F..%2Fconfig.json",           // encoded traversal
		"%2Fetc%2Fpasswd",                 // absolute path
		"..",                              // parent directory
		"pre-restore-a%5C..%5Cb.json",     // encoded backslash traversal
	}
	for _, id := range paths {
		t.Run(id, func(t *testing.T) {
			status, body := e.postRaw(e.c, "/api/admin/backups/snapshots/"+id+"/preview", "application/json", nil)
			if status == http.StatusOK {
				t.Fatalf("%q was accepted: %s", id, truncate(body))
			}
			ok := status == http.StatusBadRequest || status == http.StatusNotFound ||
				status == http.StatusMovedPermanently || status == http.StatusTemporaryRedirect
			if !ok {
				t.Fatalf("%q status = %d: %s", id, status, truncate(body))
			}
		})
	}
	// No traversal attempt may have created or exposed a snapshot.
	if status, body := e.getAs(e.c, "/api/admin/backups/snapshots"); status != http.StatusOK || !strings.Contains(string(body), `"snapshots":[]`) {
		t.Fatalf("list %d: %s", status, truncate(body))
	}
}

// ---- 8. auth gates ----

func TestBackupEndpointsRequireSession(t *testing.T) {
	e := newEnv(t)
	anon := e.newClient()
	for _, tc := range []struct {
		method string
		path   string
		body   []byte
	}{
		{"POST", "/api/admin/backup", []byte(`{"encrypted":false}`)},
		{"POST", "/api/admin/restore/preview", []byte(`{}`)},
		{"POST", "/api/admin/restore/apply", []byte(`{"preview_id":"x"}`)},
		{"GET", "/api/admin/backups/snapshots", nil},
		{"POST", "/api/admin/backups/snapshots/pre-restore-x.json/preview", []byte(`{}`)},
	} {
		req, _ := http.NewRequest(tc.method, e.ts.URL+tc.path, bytes.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", auth.CSRFValue)
		resp, err := anon.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401: %s", tc.method, tc.path, resp.StatusCode, truncate(body))
		}
		if strings.Contains(string(body), "ciphertext") || strings.Contains(string(body), "payload") {
			t.Fatalf("%s %s leaked backup data", tc.method, tc.path)
		}
	}
}

func TestBackupEndpointsRequireCSRF(t *testing.T) {
	e := newEnv(t)
	e.login()
	resp, err := e.c.Post(e.ts.URL+"/api/admin/backup", "application/json",
		strings.NewReader(`{"encrypted":false,"confirm_unencrypted":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF header accepted: %d", resp.StatusCode)
	}
}

// TestRestorePreviewErrorMapping checks the wire contract for the failures an admin can cause.
func TestRestorePreviewErrorMapping(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connectNamed("app-key-1", "Main", "/main")
	env, raw, _ := e.exportBackup(e.c, map[string]any{"encrypted": true, "password": "correct horse battery"})

	decode := func(t *testing.T, body []byte) (string, string) {
		t.Helper()
		var res struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatalf("error body: %v (%s)", err, truncate(body))
		}
		return res.Code, res.Error
	}

	t.Run("missing password", func(t *testing.T) {
		status, body := e.postMultipart(e.c, "/api/admin/restore/preview", raw, "")
		if status != http.StatusBadRequest {
			t.Fatalf("status %d: %s", status, truncate(body))
		}
		var res struct {
			Error          string `json:"error"`
			Code           string `json:"code"`
			FormatVersion  int    `json:"format_version"`
			CreatedAt      string `json:"created_at"`
			AppVersion     string `json:"app_version"`
			Encrypted      bool   `json:"encrypted"`
			EncryptionMode string `json:"encryption_mode"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatal(err)
		}
		if res.Code != "password_required" {
			t.Fatalf("code = %q", res.Code)
		}
		// The header travels with the error so the UI knows what it is unlocking.
		if res.FormatVersion != backup.FormatVersion || res.CreatedAt == "" || res.AppVersion == "" ||
			!res.Encrypted || res.EncryptionMode != backup.ModePassword {
			t.Fatalf("header fields = %+v", res)
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		status, body := e.postMultipart(e.c, "/api/admin/restore/preview", raw, "wrong horse battery")
		if status != http.StatusBadRequest {
			t.Fatalf("status %d: %s", status, truncate(body))
		}
		code, msg := decode(t, body)
		if code != "decrypt_failed" || msg != backup.ErrDecrypt.Error() {
			t.Fatalf("code = %q, message = %q", code, msg)
		}
		// The failure must not describe what the file contains.
		if strings.Contains(string(body), env.Ciphertext) || strings.Contains(string(body), "app_secret") {
			t.Fatal("error body leaks backup content")
		}
	})
	t.Run("newer format version", func(t *testing.T) {
		env.FormatVersion = backup.MaxFormatVersion + 1
		future, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		status, body := e.postMultipart(e.c, "/api/admin/restore/preview", future, "correct horse battery")
		if status != http.StatusBadRequest {
			t.Fatalf("status %d: %s", status, truncate(body))
		}
		if code, msg := decode(t, body); code != "format_version_unsupported" || !strings.Contains(msg, "버전") {
			t.Fatalf("code = %q, message = %q", code, msg)
		}
	})
	t.Run("not a backup file", func(t *testing.T) {
		status, body := e.postMultipart(e.c, "/api/admin/restore/preview", []byte(`{"hello":"world"}`), "")
		if status != http.StatusBadRequest {
			t.Fatalf("status %d: %s", status, truncate(body))
		}
		if code, _ := decode(t, body); code != "invalid_format" {
			t.Fatalf("code = %q", code)
		}
	})
	t.Run("empty upload", func(t *testing.T) {
		status, _ := e.postRaw(e.c, "/api/admin/restore/preview", "application/json", nil)
		if status != http.StatusBadRequest {
			t.Fatalf("status %d", status)
		}
	})
}

func TestBackupRefusesUnreadableCredentials(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connectNamed("app-key-1", "Main", "/main")
	// Simulate a rotated secret.key: the stored credentials no longer decrypt.
	if _, err := e.srv.DB.Exec(`UPDATE storages SET app_secret_enc = x'00010203'`); err != nil {
		t.Fatal(err)
	}
	_, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": true, "password": "correct horse battery"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("export status %d, want 500: %s", resp.StatusCode, truncate(raw))
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "credential_decrypt_failed" || !strings.Contains(body.Error, "/main") {
		t.Fatalf("error = %+v", body)
	}
	if strings.Contains(body.Error, "00010203") {
		t.Fatal("error leaks ciphertext")
	}
}

// TestRestoreInvalidatesPreRestoreClientCallbacks runs the real apply path and then invokes the
// reauth callback of a client obtained before the restore. The restored row and its fresh client
// must survive: the callback belongs to the state the restore replaced.
func TestRestoreInvalidatesPreRestoreClientCallbacks(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := e.connectNamed("app-key-old", "Main", "/main")
	oldClient, err := e.srv.St.Client(id)
	if err != nil {
		t.Fatal(err)
	}
	_, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status %d: %s", resp.StatusCode, truncate(raw))
	}
	e.previewAndApply(e.c, raw, "")

	newClient, err := e.srv.St.Client(id)
	if err != nil {
		t.Fatalf("restored storage client: %v", err)
	}
	if newClient == oldClient {
		t.Fatal("restore left the pre-restore client in the cache")
	}

	oldClient.OnReauthNeeded()

	var status, lastError string
	if err := e.srv.DB.QueryRow(`SELECT status, last_error FROM storages WHERE id = ?`, id).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != storage.StatusActive || lastError != "" {
		t.Fatalf("stale callback changed the restored row to status %q, last_error %q", status, lastError)
	}
	if got, err := e.srv.St.Client(id); err != nil || got != newClient {
		t.Fatalf("stale callback changed the restored client cache: got %p (err %v), want %p", got, err, newClient)
	}
}
