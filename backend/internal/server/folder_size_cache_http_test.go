package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type folderSizeDone struct {
	Bytes   int64 `json:"bytes"`
	Files   int64 `json:"files"`
	Folders int64 `json:"folders"`
	Done    bool  `json:"done"`
}

// folderSizeHTTP reads one folder-size SSE response and returns its done payload. A missing done
// event, an error event, or a non-SSE response fails the test instead of counting as a result.
func folderSizeHTTP(t *testing.T, e *env, vpath string) folderSizeDone {
	t.Helper()
	resp, err := e.c.Get(e.ts.URL + "/api/fs/folder-size?path=" + url.QueryEscape(vpath))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("folder-size %s: status %d: %s", vpath, resp.StatusCode, truncate(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("folder-size %s: content type %q, want text/event-stream", vpath, ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var done folderSizeDone
	var sawDone bool
	event := ""
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		switch event {
		case "error":
			t.Fatalf("folder-size %s reported an error event: %s", vpath, data)
		case "done":
			if err := json.Unmarshal([]byte(data), &done); err != nil {
				t.Fatalf("folder-size %s done payload: %v (%s)", vpath, err, data)
			}
			sawDone = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read folder-size %s stream: %v", vpath, err)
	}
	if !sawDone {
		t.Fatalf("folder-size %s stream ended without a done event", vpath)
	}
	return done
}

// TestFolderSizeHTTPSecondReadKeepsCachedCounts calls the real folder-size route twice on one
// folder. The done payload must report the same exact bytes, files and folders both times, and the
// second read must come from the cache without another Dropbox listing. The list route must
// serialize the cached bytes for the folder.
func TestFolderSizeHTTPSecondReadKeepsCachedCounts(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("size-cache", "/m")
	acct := e.fake.Account("size-cache")
	mod := time.Unix(1700000000, 0).UTC()
	acct.Put("/big/a.txt", []byte("abc"), mod)
	acct.Put("/big/sub1/b.txt", []byte("defg"), mod)
	acct.Put("/big/sub2/c.txt", []byte("0123456789"), mod)

	want := folderSizeDone{Bytes: 17, Files: 3, Folders: 2, Done: true}
	first := folderSizeHTTP(t, e, "/m/big")
	if first != want {
		t.Fatalf("first folder-size done = %+v, want %+v", first, want)
	}

	var listed struct {
		Entries []struct {
			Name string
			Size *int64
		}
	}
	if status := e.do("GET", "/api/fs/list?path=/m", nil, &listed); status != http.StatusOK {
		t.Fatalf("list /m: %d", status)
	}
	var size *int64
	for _, en := range listed.Entries {
		if en.Name == "big" {
			size = en.Size
		}
	}
	if size == nil || *size != want.Bytes {
		t.Fatalf("list /m folder size = %v, want %d from the folder-size cache", size, want.Bytes)
	}

	before := e.fake.CallCount("files/list_folder") + e.fake.CallCount("files/list_folder/continue")
	second := folderSizeHTTP(t, e, "/m/big")
	if second != first {
		t.Fatalf("second folder-size done = %+v, want the cached counts %+v", second, first)
	}
	if calls := e.fake.CallCount("files/list_folder") + e.fake.CallCount("files/list_folder/continue") - before; calls != 0 {
		t.Fatalf("cached folder-size made %d Dropbox listing calls, want 0", calls)
	}

	if status := e.do("GET", "/api/fs/list?path=/m", nil, &listed); status != http.StatusOK {
		t.Fatalf("list /m after the cached read: %d", status)
	}
	size = nil
	for _, en := range listed.Entries {
		if en.Name == "big" {
			size = en.Size
		}
	}
	if size == nil || *size != want.Bytes {
		t.Fatalf("list /m folder size after the cached read = %v, want %d", size, want.Bytes)
	}
}
