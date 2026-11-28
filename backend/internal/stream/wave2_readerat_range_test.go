package stream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A response body longer than its Content-Range declares is a framing error. A ReadAt smaller
// than one block fills the caller's buffer from the copied bytes and used to return n, nil,
// hiding the error. The error must surface even when the destination is full.
func TestReaderAtSurfacesBodyBeyondDeclaredRangeOnFullRead(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "0123456789")
	}))
	defer ts.Close()
	ra := NewReaderAt(context.Background(), ts.Client(), 10,
		func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)

	p := make([]byte, 4)
	n, err := ra.ReadAt(p, 0)
	if err == nil || !strings.Contains(err.Error(), "body exceeds declared range") {
		t.Fatalf("4-byte ReadAt over a body past its declared range: n=%d err=%v, want body exceeds declared range", n, err)
	}
	if n != 4 || string(p) != "0123" {
		t.Fatalf("ReadAt returned n=%d body=%q, want the 4 declared bytes", n, p[:n])
	}
}
