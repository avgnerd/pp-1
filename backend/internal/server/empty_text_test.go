package server

import (
	"net/http"
	"testing"
	"time"
)

// An empty text file must preview as empty text. Asking Dropbox for a byte range of a zero-length
// file is unsatisfiable there (416, which this server maps to 502), so the preview must answer
// from the size it already knows instead of downloading.
func TestTextPreviewOfEmptyFileReturnsEmptyText(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.connect("app1", "/t")
	e.fake.Account("app1").Put("/empty.txt", nil, time.Now())

	var res struct {
		Text      string `json:"text"`
		Encoding  string `json:"encoding"`
		Truncated bool   `json:"truncated"`
		Size      int64  `json:"size"`
	}
	status := e.do("GET", "/api/fs/text?path=/t/empty.txt", nil, &res)
	if status != http.StatusOK {
		t.Fatalf("empty text preview status = %d, want 200", status)
	}
	if res.Text != "" || res.Encoding != "utf-8" || res.Truncated || res.Size != 0 {
		t.Fatalf("empty text preview = %+v, want empty utf-8 body of size 0", res)
	}
	if n := e.fake.CallCount("files/download"); n != 0 {
		t.Fatalf("empty text preview made %d download calls, want 0", n)
	}
}
