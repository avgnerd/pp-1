package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/cache"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/vfs"
)

func TestSharedTempLinkIsolatedFromCallerCancel(t *testing.T) {
	c := cache.New(1000, 1<<20, time.Hour)
	defer c.Stop()
	s := &Streamer{Cache: c}
	key := vfs.TempLinkKey(7, "/cancel.bin")

	var calls atomic.Int32
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	load := func(ctx context.Context) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			started <- ctx
			select {
			case <-release:
				return "link-1", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return fmt.Sprintf("link-%d", n), nil
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.sharedTempLink(firstCtx, key, load)
		firstDone <- err
	}()

	var loadCtx context.Context
	select {
	case loadCtx = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("shared load did not start")
	}
	if _, ok := loadCtx.Deadline(); !ok {
		t.Fatal("shared load context is unbounded")
	}
	cancelFirst()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first caller error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	if err := loadCtx.Err(); err != nil {
		t.Fatalf("peer cancellation stopped the shared load: %v", err)
	}

	secondDone := make(chan struct {
		link string
		err  error
	}, 1)
	go func() {
		link, err := s.sharedTempLink(context.Background(), key, load)
		secondDone <- struct {
			link string
			err  error
		}{link, err}
	}()
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("peer started a duplicate load: calls=%d", got)
	}
	close(release)

	select {
	case second := <-secondDone:
		if second.err != nil || second.link != "link-1" {
			t.Fatalf("healthy peer got link=%q err=%v", second.link, second.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("healthy peer did not receive shared result")
	}

	cancelledCtx, cancelCached := context.WithCancel(context.Background())
	cancelCached()
	if link, err := s.sharedTempLink(cancelledCtx, key, load); err != nil || link != "link-1" {
		t.Fatalf("cache hit got link=%q err=%v", link, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cache hit called loader: calls=%d", got)
	}

	c.Delete(key)
	if link, err := s.sharedTempLink(context.Background(), key, load); err != nil || link != "link-2" {
		t.Fatalf("after invalidation got link=%q err=%v", link, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("invalidation did not reload: calls=%d", got)
	}
}

type streamRoundTripFunc func(*http.Request) (*http.Response, error)

func (f streamRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type observedBody struct {
	*strings.Reader
	closed   bool
	closedCh chan struct{}
}

func (b *observedBody) Close() error {
	b.closed = true
	if b.closedCh != nil {
		close(b.closedCh)
	}
	return nil
}

func proxyFixture(t *testing.T, transport http.RoundTripper) (*Streamer, *cache.Cache, vfs.Target) {
	t.Helper()
	c := cache.New(1000, 1<<20, time.Hour)
	t.Cleanup(c.Stop)
	s := &Streamer{Cache: c, HTTP: &http.Client{Transport: transport}}
	target := vfs.Target{Storage: storage.Storage{ID: 9}, DbxPath: "/rate.bin"}
	c.Set(vfs.TempLinkKey(target.Storage.ID, target.DbxPath), "https://bytes.test/file", 30, time.Hour)
	return s, c, target
}

func proxyResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func proxyHTTPFixture(t *testing.T, upstreamURL, name string) *httptest.Server {
	t.Helper()
	c := cache.New(1000, 1<<20, time.Hour)
	t.Cleanup(c.Stop)
	target := vfs.Target{Storage: storage.Storage{ID: 902}, DbxPath: "/range/source"}
	c.Set(vfs.TempLinkKey(target.Storage.ID, target.DbxPath), upstreamURL+"/content", 64, time.Hour)
	s := &Streamer{Cache: c, HTTP: http.DefaultClient}
	proxied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Proxy(w, r, target, name, false); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(proxied.Close)
	return proxied
}

func TestProxyPreservesMultipartRangeContentType(t *testing.T) {
	const boundary = "worker-b-boundary"
	multipartBody := "--" + boundary + "\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Range: bytes 0-0/5\r\n\r\nA\r\n" +
		"--" + boundary + "\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Range: bytes 2-2/5\r\n\r\nC\r\n" +
		"--" + boundary + "--\r\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-0,2-2" {
			t.Errorf("upstream Range=%q", got)
		}
		w.Header().Set("Content-Type", "multipart/byteranges; boundary="+boundary)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, multipartBody)
	}))
	defer upstream.Close()

	proxied := proxyHTTPFixture(t, upstream.URL, "audit.mp4")
	req, err := http.NewRequest(http.MethodGet, proxied.URL+"/raw", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-0,2-2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent || string(body) != multipartBody {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/byteranges" || params["boundary"] != boundary {
		t.Fatalf("Content-Type=%q, parsed type=%q boundary=%q err=%v", resp.Header.Get("Content-Type"), mediaType, params["boundary"], err)
	}
}

func TestProxyRangeContentTypePolicy(t *testing.T) {
	t.Run("single range keeps filename MIME", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Range", "bytes 0-0/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "A")
		}))
		defer upstream.Close()
		proxied := proxyHTTPFixture(t, upstream.URL, "audit.mp4")
		resp, err := http.Get(proxied.URL + "/raw")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != "video/mp4" {
			t.Fatalf("Content-Type=%q, want video/mp4", got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != "" {
			t.Fatalf("unexpected Content-Security-Policy=%q", got)
		}
	})

	t.Run("SVG multipart keeps filename security policy", func(t *testing.T) {
		const boundary = "svg-boundary"
		multipartBody := "--" + boundary + "\r\n" +
			"Content-Type: image/svg+xml\r\n" +
			"Content-Range: bytes 0-0/1\r\n\r\nA\r\n" +
			"--" + boundary + "--\r\n"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "multipart/byteranges; boundary="+boundary)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, multipartBody)
		}))
		defer upstream.Close()
		proxied := proxyHTTPFixture(t, upstream.URL, "audit.svg")
		resp, err := http.Get(proxied.URL + "/raw")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != "multipart/byteranges; boundary="+boundary {
			t.Fatalf("Content-Type=%q, want multipart byteranges", got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != "sandbox" {
			t.Fatalf("Content-Security-Policy=%q, want sandbox", got)
		}
	})

	t.Run("invalid multipart keeps filename MIME", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "multipart/byteranges")
			w.Header().Set("Content-Range", "bytes 0-0/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "A")
		}))
		defer upstream.Close()
		proxied := proxyHTTPFixture(t, upstream.URL, "audit.mp4")
		resp, err := http.Get(proxied.URL + "/raw")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != "video/mp4" {
			t.Fatalf("Content-Type=%q, want video/mp4", got)
		}
	})

	t.Run("HTML security override remains", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Range", "bytes 0-0/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "A")
		}))
		defer upstream.Close()
		proxied := proxyHTTPFixture(t, upstream.URL, "audit.html")
		resp, err := http.Get(proxied.URL + "/raw")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Fatalf("Content-Type=%q, want text/html", got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != "sandbox" {
			t.Fatalf("Content-Security-Policy=%q, want sandbox", got)
		}
	})
}

func TestProxyRetries429AndDrainsIntermediateBody(t *testing.T) {
	rateBody := &observedBody{Reader: strings.NewReader("rate limited")}
	var attempts atomic.Int32
	s, _, target := proxyFixture(t, streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		if got := req.Header.Get("Range"); got != "bytes=0-3" {
			t.Errorf("range header = %q", got)
		}
		if n == 1 {
			h := make(http.Header)
			h.Set("Retry-After", "0")
			return &http.Response{
				StatusCode:    http.StatusTooManyRequests,
				Header:        h,
				Body:          rateBody,
				ContentLength: int64(rateBody.Len()),
				Request:       req,
			}, nil
		}
		h := make(http.Header)
		h.Set("Content-Range", "bytes 0-3/10")
		return proxyResponse(req, http.StatusPartialContent, h, "0123"), nil
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.test/raw", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	if err := s.Proxy(rec, req, target, "rate.bin", false); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "0123" {
		t.Fatalf("proxy response status=%d body=%q", rec.Code, rec.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("upstream attempts=%d, want 2", attempts.Load())
	}
	if !rateBody.closed || rateBody.Len() != 0 {
		t.Fatalf("intermediate body closed=%v unread=%d", rateBody.closed, rateBody.Len())
	}
}

func TestProxyPreservesRetryAfterOnExhaustion(t *testing.T) {
	var attempts atomic.Int32
	var bodiesMu sync.Mutex
	var bodies []*observedBody
	s, _, target := proxyFixture(t, streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		body := &observedBody{Reader: strings.NewReader("still limited")}
		bodiesMu.Lock()
		bodies = append(bodies, body)
		bodiesMu.Unlock()
		h := make(http.Header)
		h.Set("Retry-After", "0")
		return &http.Response{
			StatusCode:    http.StatusTooManyRequests,
			Header:        h,
			Body:          body,
			ContentLength: int64(body.Len()),
			Request:       req,
		}, nil
	}))

	rec := httptest.NewRecorder()
	if err := s.Proxy(rec, httptest.NewRequest(http.MethodGet, "http://example.test/raw", nil), target, "rate.bin", false); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "0" {
		t.Fatalf("Retry-After=%q, want 0", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
	if attempts.Load() != proxyMaxRateLimitRetries+1 {
		t.Fatalf("upstream attempts=%d, want %d", attempts.Load(), proxyMaxRateLimitRetries+1)
	}
	for i, body := range bodies {
		if !body.closed || body.Len() != 0 {
			t.Fatalf("response body %d closed=%v unread=%d", i, body.closed, body.Len())
		}
	}
}

func TestProxyUsesNoStoreForServerError(t *testing.T) {
	s, _, target := proxyFixture(t, streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return proxyResponse(req, http.StatusInternalServerError, make(http.Header), "upstream failed"), nil
	}))
	rec := httptest.NewRecorder()
	if err := s.Proxy(rec, httptest.NewRequest(http.MethodGet, "http://example.test/raw", nil), target, "error.bin", false); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
}

func TestProxyDoesNotRetryLargeRetryAfter(t *testing.T) {
	for _, retryAfter := range []string{"10000000000", "9223372036854775808"} {
		t.Run(retryAfter, func(t *testing.T) {
			var attempts atomic.Int32
			s, _, target := proxyFixture(t, streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if attempts.Add(1) == 1 {
					h := make(http.Header)
					h.Set("Retry-After", retryAfter)
					return proxyResponse(req, http.StatusTooManyRequests, h, "rate limited"), nil
				}
				return proxyResponse(req, http.StatusOK, make(http.Header), "retried too soon"), nil
			}))

			rec := httptest.NewRecorder()
			if err := s.Proxy(rec, httptest.NewRequest(http.MethodGet, "http://example.test/raw", nil), target, "rate.bin", false); err != nil {
				t.Fatal(err)
			}
			if attempts.Load() != 1 {
				t.Fatalf("upstream attempts=%d, want 1", attempts.Load())
			}
			if rec.Code != http.StatusTooManyRequests || rec.Body.String() != "rate limited" {
				t.Fatalf("proxy response status=%d body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After"); got != retryAfter {
				t.Fatalf("Retry-After=%q, want %q", got, retryAfter)
			}
		})
	}
}

func TestRetryAfterDelaySupportsHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	later := now.Add(5 * time.Second)
	got, ok := retryAfterDelay(later.Format(http.TimeFormat), now)
	if !ok || got != 5*time.Second {
		t.Fatalf("date Retry-After = %v, %v", got, ok)
	}
	got, ok = retryAfterDelay(now.Add(-time.Minute).Format(http.TimeFormat), now)
	if !ok || got != 0 {
		t.Fatalf("past Retry-After = %v, %v", got, ok)
	}
}

func TestRetryAfterDelaySaturatesLargeValues(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, value := range []string{"10000000000", "9223372036854775807", "9223372036854775808"} {
		got, ok := retryAfterDelay(value, now)
		if !ok || got <= proxyMaxRetryDelay {
			t.Errorf("Retry-After %q = %v, %v; want a safe delay above %v", value, got, ok, proxyMaxRetryDelay)
		}
	}
	for _, value := range []string{"-9223372036854775809", "not-a-date"} {
		if got, ok := retryAfterDelay(value, now); ok {
			t.Errorf("Retry-After %q = %v, true; want fallback", value, got)
		}
	}
}

func TestReaderAtRetriesRateLimit(t *testing.T) {
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "rate limited")
			return
		}
		w.Header().Set("Content-Range", "bytes 0-3/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abcd")
	}))
	defer upstream.Close()
	ra := NewReaderAt(context.Background(), upstream.Client(), 4,
		func(context.Context) (string, error) { return upstream.URL + "/content", nil }, nil)
	p := make([]byte, 4)
	n, err := ra.ReadAt(p, 0)
	if err != nil || n != 4 || string(p) != "abcd" {
		t.Fatalf("ReadAt after transient 429: n=%d err=%v body=%q attempts=%d", n, err, p[:n], attempts.Load())
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("upstream attempts=%d, want 2", got)
	}
}

func TestReaderAtRetriesRateLimitAndDrainsIntermediateBody(t *testing.T) {
	rateBody := &observedBody{Reader: strings.NewReader("rate limited")}
	var attempts atomic.Int32
	client := &http.Client{Transport: streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			h := make(http.Header)
			h.Set("Retry-After", "0")
			return &http.Response{
				StatusCode:    http.StatusTooManyRequests,
				Header:        h,
				Body:          rateBody,
				ContentLength: int64(rateBody.Len()),
				Request:       req,
			}, nil
		}
		h := make(http.Header)
		h.Set("Content-Range", "bytes 0-3/4")
		return proxyResponse(req, http.StatusPartialContent, h, "abcd"), nil
	})}
	ra := NewReaderAt(context.Background(), client, 4,
		func(context.Context) (string, error) { return "https://bytes.test/content", nil }, nil)
	n, err := ra.ReadAt(make([]byte, 4), 0)
	if err != nil || n != 4 {
		t.Fatalf("ReadAt after transient 429: n=%d err=%v", n, err)
	}
	if !rateBody.closed || rateBody.Len() != 0 {
		t.Fatalf("intermediate body closed=%v unread=%d", rateBody.closed, rateBody.Len())
	}
}

func TestReaderAtRateLimitRetryExhaustion(t *testing.T) {
	var bodies []*observedBody
	var attempts atomic.Int32
	client := &http.Client{Transport: streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		body := &observedBody{Reader: strings.NewReader("still limited")}
		bodies = append(bodies, body)
		h := make(http.Header)
		h.Set("Retry-After", "0")
		return &http.Response{
			StatusCode:    http.StatusTooManyRequests,
			Header:        h,
			Body:          body,
			ContentLength: int64(body.Len()),
			Request:       req,
		}, nil
	})}
	ra := NewReaderAt(context.Background(), client, 4,
		func(context.Context) (string, error) { return "https://bytes.test/content", nil }, nil)
	n, err := ra.ReadAt(make([]byte, 4), 0)
	if n != 0 || err == nil {
		t.Fatalf("exhausted read n=%d err=%v", n, err)
	}
	if got := attempts.Load(); got != proxyMaxRateLimitRetries+1 {
		t.Fatalf("upstream attempts=%d, want %d", got, proxyMaxRateLimitRetries+1)
	}
	for i, body := range bodies {
		if !body.closed || body.Len() != 0 {
			t.Fatalf("response body %d closed=%v unread=%d", i, body.closed, body.Len())
		}
	}
	if len(ra.blocks) != 0 {
		t.Fatalf("error response populated %d cached blocks", len(ra.blocks))
	}
}

func TestReaderAtDoesNotRetryLargeRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	client := &http.Client{Transport: streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			h := make(http.Header)
			h.Set("Retry-After", "10000000000")
			return proxyResponse(req, http.StatusTooManyRequests, h, "rate limited"), nil
		}
		h := make(http.Header)
		h.Set("Content-Range", "bytes 0-3/4")
		return proxyResponse(req, http.StatusPartialContent, h, "abcd"), nil
	})}
	ra := NewReaderAt(context.Background(), client, 4,
		func(context.Context) (string, error) { return "https://bytes.test/content", nil }, nil)
	n, err := ra.ReadAt(make([]byte, 4), 0)
	if n != 0 || err == nil || attempts.Load() != 1 {
		t.Fatalf("large Retry-After n=%d err=%v attempts=%d", n, err, attempts.Load())
	}
}

func TestReaderAtRateLimitRetryIsCancelable(t *testing.T) {
	firstBody := &observedBody{Reader: strings.NewReader("rate limited"), closedCh: make(chan struct{})}
	var attempts atomic.Int32
	client := &http.Client{Transport: streamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			h := make(http.Header)
			h.Set("Retry-After", "5")
			return &http.Response{
				StatusCode:    http.StatusTooManyRequests,
				Header:        h,
				Body:          firstBody,
				ContentLength: int64(firstBody.Len()),
				Request:       req,
			}, nil
		}
		h := make(http.Header)
		h.Set("Content-Range", "bytes 0-3/4")
		return proxyResponse(req, http.StatusPartialContent, h, "abcd"), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ra := NewReaderAt(ctx, client, 4,
		func(context.Context) (string, error) { return "https://bytes.test/content", nil }, nil)
	done := make(chan error, 1)
	go func() {
		_, err := ra.ReadAt(make([]byte, 4), 0)
		done <- err
	}()
	select {
	case <-firstBody.closedCh:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("429 response was not processed")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled retry error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled retry did not return")
	}
}

func TestReaderAtShortPartialIsNotCached(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Range", "bytes 0-2/3")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("abc"))
	}))
	defer ts.Close()
	ra := NewReaderAt(context.Background(), ts.Client(), 10,
		func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)

	p := make([]byte, 10)
	n, err := ra.ReadAt(p, 0)
	if n != 3 || !errors.Is(err, io.EOF) || string(p[:n]) != "abc" {
		t.Fatalf("short partial n=%d err=%v body=%q", n, err, p[:n])
	}
	if requests.Load() != 1 {
		t.Fatalf("short partial made %d requests during one read", requests.Load())
	}
	if _, cached := ra.blocks[0]; cached {
		t.Fatal("incomplete block was cached")
	}

	n, err = ra.ReadAt(make([]byte, 1), 4)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("sparse read after truncation n=%d err=%v", n, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("sparse read used an incomplete cached block: requests=%d", requests.Load())
	}
}

func TestReaderAtRejectsShortRangeWithinLargerTotal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-2/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("abc"))
	}))
	defer ts.Close()
	ra := NewReaderAt(context.Background(), ts.Client(), 10,
		func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)

	p := make([]byte, 10)
	n, err := ra.ReadAt(p, 0)
	if n != 3 || !errors.Is(err, io.ErrUnexpectedEOF) || string(p[:n]) != "abc" {
		t.Fatalf("short range n=%d err=%v body=%q", n, err, p[:n])
	}
	if _, cached := ra.blocks[0]; cached {
		t.Fatal("short range was cached as a complete block")
	}
}

func TestReaderAtHandlesIgnoredRangeAndBounds(t *testing.T) {
	data := []byte("0123456789")
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer ts.Close()
	ra := NewReaderAt(context.Background(), ts.Client(), int64(len(data)),
		func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)
	ra.blockSize = 4

	p := make([]byte, 4)
	n, err := ra.ReadAt(p, 4)
	if err != nil || n != 4 || string(p) != "4567" {
		t.Fatalf("ignored range n=%d err=%v body=%q", n, err, p)
	}
	if requests.Load() != 1 {
		t.Fatalf("ignored range requests=%d, want 1", requests.Load())
	}
	if _, err := ra.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("negative offset returned nil error")
	}
	if n, err := ra.ReadAt(nil, 0); n != 0 || err != nil {
		t.Fatalf("zero-length read n=%d err=%v", n, err)
	}
	if n, err := ra.ReadAt(make([]byte, 1), ra.Size()); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF n=%d err=%v", n, err)
	}
}

func TestReaderAtReturnsPartialDataAtEOF(t *testing.T) {
	data := []byte("0123456789")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Range") {
		case "bytes=0-3":
			w.Header().Set("Content-Range", "bytes 0-3/10")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[:4])
		case "bytes=4-7":
			w.Header().Set("Content-Range", "bytes 4-7/10")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[4:8])
		case "bytes=8-9":
			w.Header().Set("Content-Range", "bytes 8-9/10")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[8:])
		default:
			http.Error(w, "unexpected range", http.StatusRequestedRangeNotSatisfiable)
		}
	}))
	defer ts.Close()
	ra := NewReaderAt(context.Background(), ts.Client(), int64(len(data)),
		func(context.Context) (string, error) { return ts.URL + "/file", nil }, nil)
	ra.blockSize = 4

	p := make([]byte, 3)
	n, err := ra.ReadAt(p, 8)
	if n != 2 || !errors.Is(err, io.EOF) || string(p[:n]) != "89" {
		t.Fatalf("partial EOF read n=%d err=%v body=%q", n, err, p[:n])
	}
}

func TestParseContentRange(t *testing.T) {
	cr, err := parseContentRange("bytes 4-7/10")
	if err != nil || cr.start != 4 || cr.end != 7 || cr.total != 10 || !cr.hasTotal {
		t.Fatalf("range=%+v err=%v", cr, err)
	}
	cr, err = parseContentRange("bytes 4-7/*")
	if err != nil || cr.start != 4 || cr.end != 7 || cr.hasTotal {
		t.Fatalf("unknown-total range=%+v err=%v", cr, err)
	}
	for _, bad := range []string{"", "items 0-1/2", "bytes 3-2/4", "bytes 0-2/2", "bytes 0-2/3/4"} {
		if _, err := parseContentRange(bad); err == nil {
			t.Fatalf("accepted invalid Content-Range %q", bad)
		}
	}
}
