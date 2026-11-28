package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
)

// settingsValue reads the current settings through the admin API.
func (e *env) settingsValue(c *http.Client) map[string]any {
	e.t.Helper()
	status, body := e.getAs(c, "/api/admin/settings")
	if status != http.StatusOK {
		e.t.Fatalf("get settings %d: %s", status, truncate(body))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		e.t.Fatalf("decode settings: %v (%s)", err, truncate(body))
	}
	return out
}

// putSettingsValue sends a whole settings object and returns the raw response.
func (e *env) putSettingsValue(c *http.Client, value map[string]any) (int, []byte) {
	e.t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		e.t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/api/admin/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", auth.CSRFValue)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// A PUT that read the pre-restore settings while a restore holds the settings gate must be refused
// with 409 instead of writing the old values after the restore. HEAD had no settings restore gate,
// so the write returned 200 and raced the restore.
func TestSettingsPutConflictsWithRestore(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setSetting("per_page", 55)
	_, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}
	e.setSetting("per_page", 77)
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

	// This is exactly what putSettings does before it writes: read the current settings. The
	// restore has not committed yet, so they are still the pre-restore values.
	current := e.settingsValue(e.c)
	current["per_page"] = 88
	status, body := e.putSettingsValue(e.c, current)
	if status != http.StatusConflict {
		unblock()
		<-done
		t.Fatalf("settings PUT during restore = %d, want 409: %s", status, truncate(body))
	}
	var conflict struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &conflict); err != nil {
		t.Fatalf("conflict body: %v (%s)", err, truncate(body))
	}
	if conflict.Code != "restore_in_progress" || conflict.Error == "" {
		t.Fatalf("conflict body = %+v, want code restore_in_progress and a message", conflict)
	}

	unblock()
	if applyStatus := <-done; applyStatus != http.StatusOK {
		t.Fatalf("restore apply finished with %d", applyStatus)
	}

	// The stored restore value survives, and the refused PUT did not leak into memory or the DB.
	if status := e.loginAs(e.c, "admin", auth.DefaultPassword); status != http.StatusOK {
		t.Fatalf("login after restore = %d", status)
	}
	var got settings.Settings
	status, body = e.getAs(e.c, "/api/admin/settings")
	if status != http.StatusOK {
		t.Fatalf("get settings after restore %d: %s", status, truncate(body))
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.PerPage != 55 {
		t.Fatalf("per_page after restore = %d, want the restored 55 (refused PUT wrote 88)", got.PerPage)
	}
	var dbValue string
	if err := e.srv.DB.QueryRow(`SELECT value FROM settings WHERE key = 'per_page'`).Scan(&dbValue); err != nil {
		t.Fatal(err)
	}
	if dbValue != "55" {
		t.Fatalf("database per_page = %q, want \"55\"", dbValue)
	}
}
