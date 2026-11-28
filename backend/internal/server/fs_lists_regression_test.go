package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func getRaw(t *testing.T, e *env, rawURL string) (int, []byte) {
	t.Helper()
	resp, err := e.c.Get(e.ts.URL + rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// A trailing slash names a folder and must not be mistaken for a request to list its children.
// Siblings of /m/sub/ are the files beside /m/sub, not the files inside it.
func TestSiblingsTrailingSlashListsParentFiles(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")
	acct := e.fake.Account("app1")
	now := time.Now()
	acct.Put("/sub/child.txt", []byte("child"), now)
	acct.Put("/sibling.txt", []byte("sibling"), now)

	var got []struct {
		Name string `json:"name"`
	}
	status := e.do(http.MethodGet, "/api/fs/siblings?path="+url.QueryEscape("/m/sub/"), nil, &got)
	if status != http.StatusOK {
		t.Fatalf("siblings status = %d, want 200", status)
	}
	if len(got) != 1 || got[0].Name != "sibling.txt" {
		t.Fatalf("siblings of /m/sub/ = %+v, want only sibling.txt", got)
	}
}

// An invalid path must be rejected by path cleaning, not silently reduced to its parent.
func TestSiblingsRejectsInvalidPaths(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")

	cases := []struct {
		name string
		path string
		want string
	}{
		{"parent segment", "/m/..", "경로에 '..'를 사용할 수 없습니다"},
		{"control character", "/m/bad\x01name", "경로에 제어 문자를 사용할 수 없습니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body struct {
				Error string `json:"error"`
			}
			status := e.do(http.MethodGet, "/api/fs/siblings?path="+url.QueryEscape(tc.path), nil, &body)
			if status != http.StatusBadRequest {
				t.Fatalf("siblings path %q status = %d, want 400 (error %q)", tc.path, status, body.Error)
			}
			if body.Error != tc.want {
				t.Fatalf("error = %q, want %q", body.Error, tc.want)
			}
		})
	}
}

// The virtual root with no storages has no children. An empty list must serialize as [], because
// the client distinguishes an empty folder from a missing entries field.
func TestRootListWithNoStoragesSerializesEmptyEntries(t *testing.T) {
	e := newEnv(t)
	e.login()

	status, body := getRaw(t, e, "/api/fs/list?path=/")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200", status)
	}
	var decoded struct {
		Entries json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("list body %q: %v", body, err)
	}
	if string(decoded.Entries) != "[]" {
		t.Fatalf("entries = %s, want [] (body %s)", decoded.Entries, body)
	}
}

// A folder holding only subfolders has no sibling files. The navigation list must still be [].
func TestSiblingsWithoutFilesSerializesEmptyList(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/m")
	e.fake.Account("app1").Mkdir("/only")

	status, body := getRaw(t, e, "/api/fs/siblings?path="+url.QueryEscape("/m/only"))
	if status != http.StatusOK {
		t.Fatalf("siblings status = %d, want 200", status)
	}
	if got := strings.TrimSpace(string(body)); got != "[]" {
		t.Fatalf("siblings body = %s, want []", got)
	}
}
