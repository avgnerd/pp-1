package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func wave2EmptyZipArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// GET /api/fs/archive must answer an empty archive with "entries":[], not "entries":null. The
// preview iterates entries, so null crashes the page before the empty-archive notice renders.
func TestArchiveListEmptyArchiveReturnsEmptyEntriesArray(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("wave2-empty-archive", "/m")
	e.fake.Account("wave2-empty-archive").Put("/empty.zip", wave2EmptyZipArchive(t), time.Unix(1700000000, 0).UTC())

	var res struct {
		Listing struct {
			Entries   json.RawMessage `json:"entries"`
			Truncated bool            `json:"truncated"`
		} `json:"listing"`
		BudgetMB int `json:"budget_mb"`
	}
	if st := e.do("GET", "/api/fs/archive?path=/m/empty.zip", nil, &res); st != http.StatusOK {
		t.Fatalf("archive listing status = %d, want 200", st)
	}
	if got := string(res.Listing.Entries); got != "[]" {
		t.Fatalf(`listing entries = %s, want []`, got)
	}
	if res.Listing.Truncated {
		t.Fatal("empty archive reported truncated")
	}
	if res.BudgetMB == 0 {
		t.Fatal("budget_mb missing from response")
	}
}
