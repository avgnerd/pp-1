package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

type searchStorageObservation struct {
	mount            string
	status           string
	count            int
	truncated        bool
	truncatedPresent bool
}

type searchStreamObservation struct {
	paths                []string
	total                int
	storages             int
	doneTruncated        bool
	doneTruncatedPresent bool
	storage              map[string]searchStorageObservation
}

func observeSearchStream(t *testing.T, e *env, values url.Values) searchStreamObservation {
	t.Helper()
	resp, err := e.c.Get(e.ts.URL + "/api/search?" + values.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := searchStreamObservation{storage: map[string]searchStorageObservation{}}
	seenPaths := map[string]bool{}
	event := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatalf("decode %s event: %v", event, err)
		}
		switch event {
		case "results":
			entries, _ := payload["entries"].([]any)
			for _, raw := range entries {
				entry, _ := raw.(map[string]any)
				path, _ := entry["path"].(string)
				if seenPaths[path] {
					t.Fatalf("duplicate search path %q", path)
				}
				seenPaths[path] = true
				got.paths = append(got.paths, path)
			}
		case "storage":
			mount, _ := payload["mount"].(string)
			status, _ := payload["status"].(string)
			count, _ := payload["count"].(float64)
			truncated, present := payload["truncated"].(bool)
			got.storage[mount] = searchStorageObservation{
				mount: mount, status: status, count: int(count),
				truncated: truncated, truncatedPresent: present,
			}
		case "done":
			total, _ := payload["total"].(float64)
			storages, _ := payload["storages"].(float64)
			got.total, got.storages = int(total), int(storages)
			got.doneTruncated, got.doneTruncatedPresent = payload["truncated"].(bool)
		}
		if event == "done" {
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read search stream: %v", err)
	}
	return got
}

func TestSearchMarksTruncatedWhenPagedMatchesExceedPerStorageLimit(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")
	a := e.fake.Account("app1")
	for i := 0; i < 501; i++ {
		a.Put(fmt.Sprintf("/cap/cap-match-%03d.txt", i), []byte("x"), time.Now())
	}

	got := observeSearchStream(t, e, url.Values{"q": {"cap-match-"}, "kind": {"file"}})
	if initial := e.fake.CallCount("files/search_v2"); initial != 1 {
		t.Fatalf("provider search_v2 calls = %d, want 1", initial)
	}
	// The regular fake returns all matches in one page; the audit overlay pages it 200/200/101.
	if continued := e.fake.CallCount("files/search/continue_v2"); continued != 0 && continued != 2 {
		t.Fatalf("provider continue calls = %d, want 0 or 2", continued)
	}
	if len(got.paths) != 500 || got.total != 500 || got.storages != 1 {
		t.Fatalf("returned=%d total=%d storages=%d, want 500/500/1", len(got.paths), got.total, got.storages)
	}
	storage := got.storage["/m"]
	if storage.status != "done" || storage.count != 500 || !storage.truncatedPresent || !storage.truncated {
		t.Fatalf("storage event = %+v, want status=done count=500 truncated=true", storage)
	}
	if !got.doneTruncatedPresent || !got.doneTruncated {
		t.Fatalf("done event truncated present=%v value=%v, want true", got.doneTruncatedPresent, got.doneTruncated)
	}
}

func TestSearchDoesNotMarkCompleteOrFilteredCapAsTruncated(t *testing.T) {
	tests := []struct {
		name      string
		files     int
		addFolder bool
	}{
		{name: "exact 500 complete", files: 500},
		{name: "499 eligible plus filtered folder", files: 499, addFolder: true},
		{name: "500 eligible plus filtered folder", files: 500, addFolder: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.login()
			e.connect("app1", "/m")
			a := e.fake.Account("app1")
			for i := 0; i < tc.files; i++ {
				a.Put(fmt.Sprintf("/cap/cap-match-%03d.txt", i), []byte("x"), time.Now())
			}
			if tc.addFolder {
				a.Mkdir("/cap/cap-match-zzz")
			}

			got := observeSearchStream(t, e, url.Values{"q": {"cap-match-"}, "kind": {"file"}})
			if len(got.paths) != tc.files || got.total != tc.files {
				t.Fatalf("returned=%d total=%d, want %d/%d", len(got.paths), got.total, tc.files, tc.files)
			}
			storage := got.storage["/m"]
			if storage.status != "done" || storage.count != tc.files {
				t.Fatalf("storage event = %+v, want status=done count=%d", storage, tc.files)
			}
			if storage.truncatedPresent || storage.truncated {
				t.Fatalf("storage event falsely marks truncation: %+v", storage)
			}
			if got.doneTruncatedPresent || got.doneTruncated {
				t.Fatalf("done event falsely marks truncation: present=%v value=%v", got.doneTruncatedPresent, got.doneTruncated)
			}
		})
	}
}

func TestSearchAggregatesTruncationAcrossStoragesWithProviderError(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m1")
	e.connect("app2", "/m2")
	e.connect("app3", "/m3")
	for i := 0; i < 501; i++ {
		e.fake.Account("app1").Put(fmt.Sprintf("/a/mixed-match-%03d.txt", i), []byte("x"), time.Now())
	}
	for i := 0; i < 5; i++ {
		e.fake.Account("app2").Put(fmt.Sprintf("/b/mixed-match-%d.txt", i), []byte("x"), time.Now())
	}
	e.fake.Account("app3").Put("/c/mixed-match.txt", []byte("x"), time.Now())
	e.fake.FailAccounts.Store("app3", true)

	got := observeSearchStream(t, e, url.Values{"q": {"mixed-match"}, "kind": {"file"}})
	if len(got.paths) != 505 || got.total != 505 || got.storages != 3 {
		t.Fatalf("returned=%d total=%d storages=%d, want 505/505/3", len(got.paths), got.total, got.storages)
	}
	if storage := got.storage["/m1"]; storage.status != "done" || storage.count != 500 || !storage.truncated {
		t.Fatalf("truncated storage = %+v", storage)
	}
	if storage := got.storage["/m2"]; storage.status != "done" || storage.count != 5 || storage.truncatedPresent {
		t.Fatalf("complete storage = %+v", storage)
	}
	if storage := got.storage["/m3"]; storage.status != "error" {
		t.Fatalf("failed storage = %+v", storage)
	}
	if !got.doneTruncatedPresent || !got.doneTruncated {
		t.Fatalf("done event truncated present=%v value=%v, want true", got.doneTruncatedPresent, got.doneTruncated)
	}
}
