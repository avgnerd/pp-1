package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// generatedReader yields n copies of one byte without allocating the body.
type generatedReader struct {
	remaining int64
	b         byte
}

func (r *generatedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = r.b
	}
	r.remaining -= int64(len(p))
	return len(p), nil
}

// countingReader records how many bytes the server pulled from the request body.
type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

// A multipart upload far over the 10 MiB limit must be refused after reading about the limit, not
// after the server drains all 20 MiB. Draining happens when the oversize part is closed: the
// multipart reader then copies the rest of the part to EOF. The counter is the observable proof
// that the server stopped near the cap.
func TestMultipartBackupStopsReadingAtTheLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const boundary = "dvfs-test-boundary"
	const bodyBytes = 20 << 20
	prefix := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="huge.json"` + "\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	suffix := "\r\n--" + boundary + "--\r\n"
	stream := io.MultiReader(
		strings.NewReader(prefix),
		&generatedReader{remaining: bodyBytes, b: 'a'},
		strings.NewReader(suffix),
	)
	counter := &countingReader{r: stream}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/restore/preview", counter)
	c.Request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	_, _, err := readMultipartBackup(c)
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("readMultipartBackup err = %v, want errTooLarge", err)
	}
	writeBackupError(c, err)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	// The body cap is maxBackupBytes+maxJSONWrapperBytes. Reading more than that proves a path was
	// draining the part instead of refusing as soon as the file limit tripped.
	if limit := int64(maxBackupBytes + maxJSONWrapperBytes); counter.n > limit {
		t.Fatalf("server read %d bytes from a %d byte upload, want at most %d (it drained the oversize part)",
			counter.n, bodyBytes, limit)
	}
	if min := int64(maxBackupBytes); counter.n < min {
		t.Fatalf("server read %d bytes, want at least the %d byte file limit before refusing", counter.n, min)
	}
}

// The whole-body cap catches an upload whose multipart headers are large enough that the file
// limit would not be reached first. A MaxBytesReader error must map to the same 413 and stop
// reading near the wrapper cap.
func TestMultipartBackupBodyCapMapsTo413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const boundary = "dvfs-body-cap-boundary"
	const bodyBytes = 10 << 20
	// A header larger than maxJSONWrapperBytes makes the body cap trip before the file limit.
	bigName := strings.Repeat("a", maxJSONWrapperBytes+1024)
	prefix := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="` + bigName + `"` + "\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	suffix := "\r\n--" + boundary + "--\r\n"
	stream := io.MultiReader(
		strings.NewReader(prefix),
		&generatedReader{remaining: bodyBytes, b: 'a'},
		strings.NewReader(suffix),
	)
	counter := &countingReader{r: stream}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/restore/preview", counter)
	c.Request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	_, _, err := readMultipartBackup(c)
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("readMultipartBackup err = %v, want errTooLarge from the body cap", err)
	}
	writeBackupError(c, err)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	// MaxBytesReader reads one byte past the limit to detect the overflow, so allow cap+1.
	if limit := int64(maxBackupBytes + maxJSONWrapperBytes + 1); counter.n > limit {
		t.Fatalf("server read %d bytes, want at most the body cap %d", counter.n, limit)
	}
}
