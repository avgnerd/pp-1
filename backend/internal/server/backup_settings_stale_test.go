package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
)

type settingsPutResponse struct {
	status int
	body   []byte
}

// A settings PUT that reads the pre-restore values and then blocks while the restore commits must
// not write that snapshot afterwards. The partial body holds the handler between its Get and its
// Update while the restore runs; the stale revision must produce 409 and the restored value must
// remain in both memory and the database. HEAD had no restore revision, so this PUT returned 200
// and overwrote the restored per_page with 88.
func TestSettingsPutReadBeforeRestoreIsRejectedAfterCommit(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.setSetting("per_page", 55)
	_, raw, resp := e.exportBackup(e.c, map[string]any{"encrypted": false, "confirm_unencrypted": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %d: %s", resp.StatusCode, truncate(raw))
	}
	e.setSetting("per_page", 77)
	preview := e.previewOne(e.c, raw, "")

	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	req, _ := http.NewRequest(http.MethodPut, e.ts.URL+"/api/admin/settings", pr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", auth.CSRFValue)
	putDone := make(chan settingsPutResponse, 1)
	go func() {
		resp, err := e.c.Do(req)
		if err != nil {
			putDone <- settingsPutResponse{status: -1, body: []byte(err.Error())}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		putDone <- settingsPutResponse{status: resp.StatusCode, body: body}
	}()

	// An incomplete JSON value makes the handler wait after it has already read the settings.
	if _, err := io.WriteString(pw, `{"per_page":88`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if !e.srv.settingsPutMu.TryLock() {
			break // The handler holds the read-modify-write lock.
		}
		e.srv.settingsPutMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("settings PUT did not reach the handler")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // Let the handler finish Get and block in bindJSON.

	status, body := e.postJSON(e.c, "/api/admin/restore/apply", map[string]string{"preview_id": preview.PreviewID}, nil)
	if status != http.StatusOK {
		t.Fatalf("restore apply %d: %s", status, truncate(body))
	}

	if _, err := io.WriteString(pw, "}"); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	got := <-putDone
	if got.status != http.StatusConflict {
		t.Fatalf("stale settings PUT after restore = %d, want 409: %s", got.status, truncate(got.body))
	}
	var conflict struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(got.body, &conflict); err != nil {
		t.Fatalf("conflict body: %v (%s)", err, truncate(got.body))
	}
	if conflict.Code != "restore_in_progress" {
		t.Fatalf("conflict code = %q, want restore_in_progress", conflict.Code)
	}

	if status := e.loginAs(e.c, "admin", auth.DefaultPassword); status != http.StatusOK {
		t.Fatalf("login after restore = %d", status)
	}
	var current settings.Settings
	status, body = e.getAs(e.c, "/api/admin/settings")
	if status != http.StatusOK {
		t.Fatalf("get settings after restore %d: %s", status, truncate(body))
	}
	if err := json.Unmarshal(body, &current); err != nil {
		t.Fatal(err)
	}
	if current.PerPage != 55 {
		t.Fatalf("per_page after restore = %d, want the restored 55", current.PerPage)
	}
	var dbValue string
	if err := e.srv.DB.QueryRow(`SELECT value FROM settings WHERE key = 'per_page'`).Scan(&dbValue); err != nil {
		t.Fatal(err)
	}
	if dbValue != "55" {
		t.Fatalf("database per_page = %q, want \"55\"", dbValue)
	}
}
