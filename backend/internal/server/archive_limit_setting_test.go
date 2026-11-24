package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// sizedTgz builds a .tar.gz whose second entry lies past a 1 MB read but within 4 MB: the listing
// under a 1 MB budget stops before it, so the two budgets produce visibly different listings.
func sizedTgz(t *testing.T) []byte {
	t.Helper()
	big := make([]byte, 1500<<10)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o600, Size: int64(len(big))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(big); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "after.txt", Mode: 0o600, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestArchiveListFollowsArchiveLimitChange pins that the listing served by archiveList answers for
// the archive_max_mb in force when it is served: a cached listing cut off under the old limit must
// not be served after the admin raises the limit, which is exactly the advice the truncation notice
// gives.
func TestArchiveListFollowsArchiveLimitChange(t *testing.T) {
	s, fs, _ := proxyArchiveEnv(t, "arc-limit")
	fs.Account("arc-limit").Put("/a.tgz", sizedTgz(t), time.Unix(1700000000, 0).UTC())

	n := s.Settings.Get()
	n.ArchiveMaxMB = 1
	if err := s.Settings.Update(n); err != nil {
		t.Fatal(err)
	}
	type reply struct {
		Listing struct {
			Entries   []struct{ Name string } `json:"entries"`
			Truncated bool                    `json:"truncated"`
		} `json:"listing"`
		BudgetMB int `json:"budget_mb"`
	}
	decode := func() reply {
		t.Helper()
		got := awaitReply(t, runArchiveList(s, context.Background(), "/m/a.tgz"), "archiveList")
		if got.status != http.StatusOK {
			t.Fatalf("status = %d body %q, want the listing", got.status, got.body)
		}
		var out reply
		if err := json.Unmarshal([]byte(got.body), &out); err != nil {
			t.Fatalf("decode listing %q: %v", got.body, err)
		}
		return out
	}

	capped := decode()
	if !capped.Listing.Truncated || len(capped.Listing.Entries) != 1 || capped.BudgetMB != 1 {
		t.Fatalf("listing under 1 MB = %+v, want one entry truncated", capped)
	}

	n = s.Settings.Get()
	n.ArchiveMaxMB = 4
	if err := s.Settings.Update(n); err != nil {
		t.Fatal(err)
	}
	raised := decode()
	if raised.BudgetMB != 4 {
		t.Fatalf("budget_mb = %d, want 4", raised.BudgetMB)
	}
	if raised.Listing.Truncated || len(raised.Listing.Entries) != 2 {
		t.Fatalf("listing under 4 MB = %+v, want both entries untruncated", raised.Listing)
	}
	names := map[string]bool{}
	for _, e := range raised.Listing.Entries {
		names[e.Name] = true
	}
	if !names["big.bin"] || !names["after.txt"] {
		t.Fatalf("listing under 4 MB = %+v, want big.bin and after.txt", raised.Listing.Entries)
	}
}
