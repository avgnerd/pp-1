package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type readerAtRequest struct {
	rng     string
	ifRange string
}

func testRangeBounds(w http.ResponseWriter, r *http.Request, size int) (int, int, bool) {
	rng := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	parts := strings.SplitN(rng, "-", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid request Range", http.StatusBadRequest)
		return 0, 0, false
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		http.Error(w, "invalid request Range start", http.StatusBadRequest)
		return 0, 0, false
	}
	end, err := strconv.Atoi(parts[1])
	if err != nil || end < start || end >= size {
		http.Error(w, "invalid request Range", http.StatusBadRequest)
		return 0, 0, false
	}
	return start, end, true
}

func writeTestPartial(w http.ResponseWriter, r *http.Request, data []byte, etag string) {
	start, end, ok := testRangeBounds(w, r, len(data))
	if !ok {
		return
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

func TestReaderAtPinsStrongETagAcrossBlocks(t *testing.T) {
	data := []byte("abcd")
	var mu sync.Mutex
	var requests []readerAtRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, readerAtRequest{rng: r.Header.Get("Range"), ifRange: r.Header.Get("If-Range")})
		mu.Unlock()
		writeTestPartial(w, r, data, `"rev1"`)
	}))
	defer ts.Close()

	ra := NewReaderAt(context.Background(), ts.Client(), int64(len(data)),
		func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)
	ra.blockSize = 2
	p := make([]byte, 4)
	n, err := ra.ReadAt(p, 0)
	if err != nil || n != 4 || string(p) != "abcd" {
		t.Fatalf("ReadAt n=%d err=%v body=%q", n, err, p[:n])
	}
	mu.Lock()
	got := append([]readerAtRequest(nil), requests...)
	mu.Unlock()
	if len(got) != 2 || got[0].ifRange != "" || got[1].ifRange != `"rev1"` {
		t.Fatalf("requests=%+v, want first unpinned and second If-Range rev1", got)
	}
}

func TestReaderAtRejectsChangedStrongETag(t *testing.T) {
	data := []byte("abcd")
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		start, _, ok := testRangeBounds(w, r, len(data))
		if !ok {
			return
		}
		etag := `"rev1"`
		blockData := data
		if start == 2 {
			etag = `"rev2"`
			blockData = []byte("abCD")
		}
		writeTestPartial(w, r, blockData, etag)
	}))
	defer ts.Close()

	ra := NewReaderAt(context.Background(), ts.Client(), int64(len(data)),
		func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)
	ra.blockSize = 2
	p := make([]byte, 4)
	n, firstErr := ra.ReadAt(p, 0)
	if n != 2 || firstErr == nil || string(p[:n]) != "ab" {
		t.Fatalf("changed read n=%d err=%v body=%q", n, firstErr, p[:n])
	}
	if _, cached := ra.blocks[1]; cached || len(ra.blocks) != 1 {
		t.Fatalf("changed block was cached: blocks=%d", len(ra.blocks))
	}

	n, nextErr := ra.ReadAt(make([]byte, 2), 0)
	if n != 0 || nextErr == nil || nextErr.Error() != firstErr.Error() {
		t.Fatalf("cached read after change n=%d err=%v, want sticky %v", n, nextErr, firstErr)
	}
	if requests.Load() != 2 {
		t.Fatalf("sticky reader made another request: requests=%d", requests.Load())
	}
}

func TestReaderAtAllowsMatchingConditionalOK(t *testing.T) {
	data := []byte("abcd")
	var requests atomic.Int32
	var gotIfRange atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := requests.Add(1)
		if call == 1 {
			writeTestPartial(w, r, data, `"rev1"`)
			return
		}
		gotIfRange.Store(r.Header.Get("If-Range"))
		w.Header().Set("ETag", `"rev1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer ts.Close()

	ra := NewReaderAt(context.Background(), ts.Client(), int64(len(data)),
		func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)
	ra.blockSize = 2
	p := make([]byte, 4)
	n, err := ra.ReadAt(p, 0)
	if err != nil || n != 4 || string(p) != "abcd" {
		t.Fatalf("matching conditional 200 n=%d err=%v body=%q", n, err, p[:n])
	}
	if got, _ := gotIfRange.Load().(string); got != `"rev1"` {
		t.Fatalf("conditional 200 If-Range=%q, want rev1", got)
	}
}

func TestReaderAtRejectsUnsafeConditionalOK(t *testing.T) {
	for _, tc := range []struct {
		name string
		etag string
	}{
		{name: "changed", etag: `"rev2"`},
		{name: "omitted", etag: ""},
		{name: "weakened", etag: `W/"rev1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("abcd")
			changedData := []byte("abCD")
			var requests atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					writeTestPartial(w, r, data, `"rev1"`)
					return
				}
				if tc.etag != "" {
					w.Header().Set("ETag", tc.etag)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(changedData)
			}))
			defer ts.Close()

			ra := NewReaderAt(context.Background(), ts.Client(), int64(len(data)),
				func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)
			ra.blockSize = 2
			p := make([]byte, 4)
			n, err := ra.ReadAt(p, 0)
			if n != 2 || err == nil || string(p[:n]) != "ab" {
				t.Fatalf("unsafe conditional 200 n=%d err=%v body=%q", n, err, p[:n])
			}
			if _, cached := ra.blocks[1]; cached || len(ra.blocks) != 1 {
				t.Fatalf("unsafe conditional 200 cached bytes: blocks=%d", len(ra.blocks))
			}
		})
	}
}

func TestReaderAtRemainsUsableWithoutStrongETag(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tag          func(int) string
		lastModified bool
	}{
		{name: "none", tag: func(int) string { return "" }},
		{name: "weak", tag: func(start int) string {
			if start == 0 {
				return `W/"rev1"`
			}
			return `W/"rev2"`
		}},
		{name: "weak_then_strong", tag: func(start int) string {
			if start == 0 {
				return `W/"rev1"`
			}
			return `"rev2"`
		}},
		{name: "last_modified", tag: func(int) string { return "" }, lastModified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("abcd")
			var sawIfRange atomic.Bool
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-Range") != "" {
					sawIfRange.Store(true)
				}
				start, _, ok := testRangeBounds(w, r, len(data))
				if !ok {
					return
				}
				if tc.lastModified {
					w.Header().Set("Last-Modified", time.Date(2026, 11, 4, 0, start, 0, 0, time.UTC).Format(http.TimeFormat))
				}
				writeTestPartial(w, r, data, tc.tag(start))
			}))
			defer ts.Close()

			ra := NewReaderAt(context.Background(), ts.Client(), int64(len(data)),
				func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)
			ra.blockSize = 2
			p := make([]byte, 4)
			n, err := ra.ReadAt(p, 0)
			if err != nil || n != 4 || string(p) != "abcd" {
				t.Fatalf("validator-less read n=%d err=%v body=%q", n, err, p[:n])
			}
			if sawIfRange.Load() {
				t.Fatal("sent If-Range without a strong ETag")
			}
		})
	}
}

func TestReaderAtRetainsPinnedValidatorAcrossRefresh(t *testing.T) {
	data := []byte("abcd")
	var requests atomic.Int32
	var invalidated atomic.Int32
	var mu sync.Mutex
	var secondIfRange string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := requests.Add(1)
		start, _, ok := testRangeBounds(w, r, len(data))
		if !ok {
			return
		}
		if call == 1 {
			writeTestPartial(w, r, data, `"rev1"`)
			return
		}
		if call == 2 {
			w.Header().Set("ETag", `"rev1"`)
			w.WriteHeader(http.StatusGone)
			return
		}
		mu.Lock()
		secondIfRange = r.Header.Get("If-Range")
		mu.Unlock()
		etag := `"rev2"`
		blockData := []byte("abCD")
		if start == 0 {
			etag = `"rev1"`
			blockData = data
		}
		writeTestPartial(w, r, blockData, etag)
	}))
	defer ts.Close()

	ra := NewReaderAt(context.Background(), ts.Client(), int64(len(data)),
		func(context.Context) (string, error) { return ts.URL + "/file", nil },
		func() { invalidated.Add(1) })
	ra.blockSize = 2
	n, firstErr := ra.ReadAt(make([]byte, 4), 0)
	if n != 2 || firstErr == nil {
		t.Fatalf("refresh change n=%d err=%v", n, firstErr)
	}
	if invalidated.Load() != 1 {
		t.Fatalf("invalidate calls=%d, want 1", invalidated.Load())
	}
	mu.Lock()
	ifRange := secondIfRange
	mu.Unlock()
	if ifRange != `"rev1"` {
		t.Fatalf("If-Range after refresh=%q, want pinned rev1", ifRange)
	}
	if _, cached := ra.blocks[1]; cached || len(ra.blocks) != 1 {
		t.Fatalf("changed bytes after refresh cached: blocks=%d", len(ra.blocks))
	}
	if _, err := ra.ReadAt(make([]byte, 2), 0); err == nil || err.Error() != firstErr.Error() {
		t.Fatalf("post-refresh sticky error=%v, want %v", err, firstErr)
	}
}

func TestReaderAtConcurrentInitialResponsesNeverMixRepresentations(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	client := &http.Client{Transport: streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		rng := req.Header.Get("Range")
		started <- rng
		<-release
		start := 0
		etag := `"rev1"`
		body := []byte("ab")
		if rng == "bytes=2-3" {
			start = 2
			etag = `"rev2"`
			body = []byte("CD")
		}
		h := make(http.Header)
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/4", start, start+len(body)-1))
		h.Set("ETag", etag)
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        h,
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       req,
		}, nil
	})}
	ra := NewReaderAt(context.Background(), client, 4,
		func(context.Context) (string, error) { return "https://bytes.test/content", nil }, nil)
	ra.blockSize = 2

	type result struct {
		n   int
		err error
	}
	results := make(chan result, 2)
	for _, off := range []int64{0, 2} {
		go func(off int64) {
			n, err := ra.ReadAt(make([]byte, 2), off)
			results <- result{n: n, err: err}
		}(off)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("concurrent range requests did not start")
		}
	}
	close(release)

	successes, failures := 0, 0
	for range 2 {
		select {
		case result := <-results:
			if result.err == nil && result.n == 2 {
				successes++
			} else if result.err != nil {
				failures++
			} else {
				t.Fatalf("unexpected concurrent result n=%d err=%v", result.n, result.err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent reads did not complete")
		}
	}
	if failures == 0 || successes > 1 {
		t.Fatalf("concurrent successes=%d failures=%d, want a detected conflict", successes, failures)
	}
	if len(ra.blocks) > 1 {
		t.Fatalf("concurrent reads cached %d blocks, want at most one representation", len(ra.blocks))
	}
	for _, block := range ra.blocks {
		if got := string(block); got != "ab" && got != "CD" {
			t.Fatalf("concurrent read cached mixed or unexpected bytes %q", got)
		}
	}
	if _, err := ra.ReadAt(make([]byte, 2), 0); err == nil {
		t.Fatal("reader did not retain the concurrent representation conflict")
	}
}
