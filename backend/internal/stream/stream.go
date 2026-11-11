// Package stream serves file bytes: temporary-link redirects (direct mode), Range proxying
// (proxy mode), signed URLs for external players, and a Range-backed io.ReaderAt.
package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/cache"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/vfs"
)

// TempLinkTTL is shorter than Dropbox's four-hour link lifetime so cached links never expire mid-use.
const TempLinkTTL = 3*time.Hour + 30*time.Minute

const tempLinkLoadTimeout = 30 * time.Second

// bufPool holds 1 MiB copy buffers; at 100 Mbps that is ~80 ms of data per syscall batch.
var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// Streamer serves file content.
type Streamer struct {
	St    *storage.Manager
	Cache *cache.Cache
	Box   *secret.Box
	HTTP  *http.Client

	linkMu    sync.Mutex
	linkLoads map[string]*tempLinkCall
}

type tempLinkCall struct {
	done chan struct{}
	link string
	err  error
}

// New creates a streamer.
func New(st *storage.Manager, c *cache.Cache, box *secret.Box) *Streamer {
	return &Streamer{St: st, Cache: c, Box: box, HTTP: dropbox.HTTPClient}
}

// TempLink returns a cached temporary link for a file.
func (s *Streamer) TempLink(ctx context.Context, t vfs.Target) (string, error) {
	key := vfs.TempLinkKey(t.Storage.ID, t.DbxPath)
	link, err := s.sharedTempLink(ctx, key, func(ctx context.Context) (string, error) {
		c, err := s.St.Client(t.Storage.ID)
		if err != nil {
			return "", err
		}
		l, err := c.GetTemporaryLink(ctx, t.DbxPath)
		if err != nil {
			return "", err
		}
		return l.Link, nil
	})
	if err != nil {
		if dropbox.HasSummary(err, "not_found") {
			return "", vfs.ErrNotFound
		}
		return "", err
	}
	return link, nil
}

func (s *Streamer) sharedTempLink(ctx context.Context, key string, load func(context.Context) (string, error)) (string, error) {
	if v, ok := s.Cache.Get(key); ok {
		if link, ok := v.(string); ok {
			return link, nil
		}
	}

	s.linkMu.Lock()
	if v, ok := s.Cache.Get(key); ok {
		if link, ok := v.(string); ok {
			s.linkMu.Unlock()
			return link, nil
		}
	}
	if call := s.linkLoads[key]; call != nil {
		s.linkMu.Unlock()
		return waitTempLink(ctx, call)
	}
	if err := ctx.Err(); err != nil {
		s.linkMu.Unlock()
		return "", err
	}
	call := &tempLinkCall{done: make(chan struct{})}
	if s.linkLoads == nil {
		s.linkLoads = make(map[string]*tempLinkCall)
	}
	s.linkLoads[key] = call
	s.linkMu.Unlock()

	go s.loadTempLink(key, call, load)
	return waitTempLink(ctx, call)
}

func (s *Streamer) loadTempLink(key string, call *tempLinkCall, load func(context.Context) (string, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), tempLinkLoadTimeout)
	defer cancel()
	link, err := load(ctx)
	if err == nil {
		s.Cache.Set(key, link, int64(len(link)), TempLinkTTL)
	}

	s.linkMu.Lock()
	call.link, call.err = link, err
	if s.linkLoads[key] == call {
		delete(s.linkLoads, key)
	}
	close(call.done)
	s.linkMu.Unlock()
}

func waitTempLink(ctx context.Context, call *tempLinkCall) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-call.done:
		return call.link, call.err
	}
}

// ContentType guesses a MIME type from the file name, with fixes for common media types
// that Windows' registry often lacks.
func ContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".opus":
		return "audio/ogg; codecs=opus"
	case ".wav":
		return "audio/wav"
	case ".pdf":
		return "application/pdf"
	case ".txt", ".log", ".md", ".srt", ".vtt", ".ass", ".ini", ".cfg":
		return "text/plain; charset=utf-8"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".svg":
		return "image/svg+xml"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// ContentDisposition builds an RFC 6266 header with an ASCII fallback and UTF-8 filename*.
func ContentDisposition(kind, name string) string {
	ascii := make([]rune, 0, len(name))
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			ascii = append(ascii, '_')
		} else {
			ascii = append(ascii, r)
		}
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, string(ascii), url.PathEscape(name))
}

// hop-by-hop and headers we never forward from Dropbox.
var forward = []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"}

const (
	proxyMaxRateLimitRetries = 2
	proxyRetryBackoff        = 100 * time.Millisecond
	proxyMaxRetryDelay       = 30 * time.Second
)

// Proxy streams a file through the server, forwarding Range requests. When download is
// true the response is an attachment. Expired links are refreshed once.
func (s *Streamer) Proxy(w http.ResponseWriter, r *http.Request, t vfs.Target, name string, download bool) error {
	var link string
	refreshesRemaining := 1
	rateLimitRetries := 0
	for {
		if link == "" {
			var err error
			link, err = s.TempLink(r.Context(), t)
			if err != nil {
				return err
			}
		}
		method := http.MethodGet
		if r.Method == http.MethodHead {
			method = http.MethodHead
		}
		req, err := http.NewRequestWithContext(r.Context(), method, link, nil)
		if err != nil {
			return err
		}
		for _, h := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		resp, err := s.HTTP.Do(req)
		if err != nil {
			return err
		}
		if (resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) && refreshesRemaining > 0 {
			dropbox.Drain(resp.Body)
			s.Cache.Delete(vfs.TempLinkKey(t.Storage.ID, t.DbxPath))
			link = ""
			refreshesRemaining--
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests && rateLimitRetries < proxyMaxRateLimitRetries {
			delay, ok := retryAfterDelay(resp.Header.Get("Retry-After"), time.Now())
			if !ok {
				delay = proxyRetryBackoff * time.Duration(1<<rateLimitRetries)
			}
			if delay <= proxyMaxRetryDelay {
				dropbox.Drain(resp.Body)
				rateLimitRetries++
				if err := waitProxyRetry(r.Context(), delay); err != nil {
					return err
				}
				continue
			}
		}
		defer resp.Body.Close()
		h := w.Header()
		for _, k := range forward {
			if v := resp.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if v := resp.Header.Get("Retry-After"); v != "" {
				h.Set("Retry-After", v)
			}
		}
		contentType := ContentType(name)
		if resp.StatusCode == http.StatusPartialContent {
			if upstreamType := resp.Header.Get("Content-Type"); upstreamType != "" {
				if mediaType, params, err := mime.ParseMediaType(upstreamType); err == nil && mediaType == "multipart/byteranges" && params["boundary"] != "" {
					contentType = upstreamType
				}
			}
		}
		h.Set("Content-Type", contentType)
		if h.Get("Accept-Ranges") == "" {
			h.Set("Accept-Ranges", "bytes")
		}
		kind := "inline"
		if download {
			kind = "attachment"
		}
		h.Set("Content-Disposition", ContentDisposition(kind, name))
		if resp.StatusCode >= http.StatusBadRequest {
			h.Set("Cache-Control", "no-store")
		} else {
			h.Set("Cache-Control", "private, max-age=3600")
		}
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(ContentType(name), "text/html") || strings.HasPrefix(ContentType(name), "image/svg") {
			h.Set("Content-Security-Policy", "sandbox")
		}
		w.WriteHeader(resp.StatusCode)
		if method == http.MethodHead {
			return nil
		}
		bp := bufPool.Get().(*[]byte)
		defer bufPool.Put(bp)
		_, err = io.CopyBuffer(w, resp.Body, *bp)
		if err != nil && r.Context().Err() != nil {
			return nil // client went away
		}
		return err
	}
}

func retryAfterDelay(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds > int64((1<<63-1)/time.Second) {
			return proxyMaxRetryDelay + time.Second, true
		}
		return time.Duration(seconds) * time.Second, true
	} else if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(value, "-") {
		return proxyMaxRetryDelay + time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func waitProxyRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SignedPath returns "/api/s/raw/<exp>/<sig>/<escaped vpath>" valid until exp.
func (s *Streamer) SignedPath(vpath string, ttl time.Duration) string {
	exp := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	sig := s.Box.Sign("raw|" + exp + "|" + vpath)
	segs := strings.Split(strings.TrimPrefix(vpath, "/"), "/")
	for i, sg := range segs {
		segs[i] = url.PathEscape(sg)
	}
	return "/api/s/raw/" + exp + "/" + sig + "/" + strings.Join(segs, "/")
}

// VerifySigned checks exp and sig for vpath.
func (s *Streamer) VerifySigned(vpath, exp, sig string) bool {
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > n {
		return false
	}
	return s.Box.Verify("raw|"+exp+"|"+vpath, sig)
}

// ReaderAt reads a remote file with HTTP Range requests through fixed-size cached blocks.
// The first accepted range-request response decides how blocks are bound. If that response
// supplies a strong ETag, all blocks are pinned to it. If it supplies only a weak ETag or
// none, the reader remains unverifiable for its lifetime; later strong ETags are not trusted
// to retroactively bind earlier blocks. Such upstreams remain usable, but ReaderAt cannot
// prove that their blocks came from one representation.
type ReaderAt struct {
	ctx       context.Context
	link      func(ctx context.Context) (string, error)
	invalid   func()
	size      int64
	blockSize int64
	maxBlocks int
	http      *http.Client

	mu             sync.Mutex
	blocks         map[int64][]byte
	order          []int64
	representation readerAtRepresentation
	validator      string
	failure        error
	Reads          int // number of HTTP requests made (for tests)
}

type readerAtRepresentation uint8

const (
	readerAtRepresentationUnknown readerAtRepresentation = iota
	readerAtRepresentationPinned
	readerAtRepresentationUnverifiable
)

var errReaderAtRepresentationChanged = errors.New("range read: representation changed")

// NewReaderAt creates a range reader. link returns the current URL; invalidate drops it.
func NewReaderAt(ctx context.Context, h *http.Client, size int64, link func(context.Context) (string, error), invalidate func()) *ReaderAt {
	return &ReaderAt{ctx: ctx, http: h, size: size, link: link, invalid: invalidate, blockSize: 256 << 10, maxBlocks: 64, blocks: map[int64][]byte{}}
}

// Size returns the file size.
func (r *ReaderAt) Size() int64 { return r.size }

// ReadAt implements io.ReaderAt.
func (r *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.representationFailure(); err != nil {
		return 0, err
	}
	if off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < r.size {
		bi := off / r.blockSize
		b, err := r.block(bi)
		within := off - bi*r.blockSize
		c := 0
		if within < int64(len(b)) {
			c = copy(p[n:], b[within:])
		}
		if c == 0 {
			if err != nil {
				return n, err
			}
			return n, io.EOF
		}
		n += c
		off += int64(c)
		if err != nil {
			if n == len(p) {
				return n, nil
			}
			return n, err
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *ReaderAt) block(i int64) ([]byte, error) {
	if i < 0 {
		return nil, errors.New("negative block index")
	}
	r.mu.Lock()
	if r.failure != nil {
		err := r.failure
		r.mu.Unlock()
		return nil, err
	}
	if b, ok := r.blocks[i]; ok {
		r.mu.Unlock()
		return b, nil
	}
	r.mu.Unlock()
	start := i * r.blockSize
	end := start + r.blockSize - 1
	if end >= r.size {
		end = r.size - 1
	}
	requested := end - start + 1
	refreshesRemaining := 1
	rateLimitRetries := 0
	for {
		link, err := r.link(r.ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, link, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		if validator, err := r.conditionalValidator(); err != nil {
			return nil, err
		} else if validator != "" {
			req.Header.Set("If-Range", validator)
		}
		resp, err := r.http.Do(req)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.Reads++
		r.mu.Unlock()
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			dropbox.Drain(resp.Body)
			if refreshesRemaining > 0 && r.invalid != nil {
				r.invalid()
				refreshesRemaining--
				continue
			}
			return nil, fmt.Errorf("range read: HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			dropbox.Drain(resp.Body)
			if rateLimitRetries < proxyMaxRateLimitRetries {
				delay, ok := retryAfterDelay(resp.Header.Get("Retry-After"), time.Now())
				if !ok {
					delay = proxyRetryBackoff * time.Duration(1<<rateLimitRetries)
				}
				if delay <= proxyMaxRetryDelay {
					rateLimitRetries++
					if err := waitProxyRetry(r.ctx, delay); err != nil {
						return nil, err
					}
					continue
				}
			}
			return nil, fmt.Errorf("range read: HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			dropbox.Drain(resp.Body)
			return nil, fmt.Errorf("range read: HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusPartialContent {
			contentRange, err := parseContentRange(resp.Header.Get("Content-Range"))
			if err != nil {
				dropbox.Drain(resp.Body)
				return nil, err
			}
			if contentRange.start != start {
				dropbox.Drain(resp.Body)
				return nil, fmt.Errorf("range read: response starts at %d, want %d", contentRange.start, start)
			}
			if contentRange.end > end {
				dropbox.Drain(resp.Body)
				return nil, fmt.Errorf("range read: response ends at %d, beyond requested %d", contentRange.end, end)
			}
			if err := r.observeRepresentation(resp.Header); err != nil {
				resp.Body.Close()
				return nil, err
			}
			expected := contentRange.end - contentRange.start + 1
			data, err := readResponseBody(resp.Body, expected, true)
			resp.Body.Close()
			if err != nil {
				return data, err
			}
			if expected < requested {
				if contentRange.hasTotal && contentRange.total <= r.size && contentRange.end == contentRange.total-1 {
					return data, io.EOF
				}
				return data, io.ErrUnexpectedEOF
			}
			if err := r.cacheBlock(i, data); err != nil {
				return nil, err
			}
			return data, nil
		}

		if err := r.observeRepresentation(resp.Header); err != nil {
			resp.Body.Close()
			return nil, err
		}
		body := io.Reader(resp.Body)
		if start > 0 {
			// Server ignored Range; skip to the block. Metadata can be stale, so reaching EOF
			// before the requested position is a legitimate end-of-file condition.
			if _, err := io.CopyN(io.Discard, body, start); err != nil {
				resp.Body.Close()
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return nil, io.EOF
				}
				return nil, err
			}
		}
		data, err := readResponseBody(body, requested, false)
		resp.Body.Close()
		if err != nil {
			return data, err
		}
		if int64(len(data)) < requested {
			return data, io.EOF
		}
		if err := r.cacheBlock(i, data); err != nil {
			return nil, err
		}
		return data, nil
	}
}

type contentRange struct {
	start, end int64
	total      int64
	hasTotal   bool
}

func parseContentRange(value string) (contentRange, error) {
	if !strings.HasPrefix(value, "bytes ") {
		return contentRange{}, fmt.Errorf("range read: invalid Content-Range %q", value)
	}
	rng := strings.TrimPrefix(value, "bytes ")
	dash := strings.IndexByte(rng, '-')
	slash := strings.IndexByte(rng, '/')
	if dash <= 0 || slash <= dash+1 || slash == len(rng)-1 {
		return contentRange{}, fmt.Errorf("range read: invalid Content-Range %q", value)
	}
	start, err := strconv.ParseInt(rng[:dash], 10, 64)
	if err != nil || start < 0 {
		return contentRange{}, fmt.Errorf("range read: invalid Content-Range %q", value)
	}
	end, err := strconv.ParseInt(rng[dash+1:slash], 10, 64)
	if err != nil || end < start {
		return contentRange{}, fmt.Errorf("range read: invalid Content-Range %q", value)
	}
	cr := contentRange{start: start, end: end}
	if total := rng[slash+1:]; total != "*" {
		cr.total, err = strconv.ParseInt(total, 10, 64)
		if err != nil || cr.total <= end {
			return contentRange{}, fmt.Errorf("range read: invalid Content-Range %q", value)
		}
		cr.hasTotal = true
	}
	return cr, nil
}

func readResponseBody(body io.Reader, want int64, exact bool) ([]byte, error) {
	limit := want
	if exact {
		limit++
	}
	b, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		return b, err
	}
	if exact && int64(len(b)) > want {
		return b[:want], fmt.Errorf("range read: body exceeds declared range")
	}
	if exact && int64(len(b)) < want {
		return b, io.ErrUnexpectedEOF
	}
	return b, nil
}

func (r *ReaderAt) conditionalValidator() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return "", r.failure
	}
	if r.representation == readerAtRepresentationPinned {
		return r.validator, nil
	}
	return "", nil
}

func (r *ReaderAt) representationFailure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func (r *ReaderAt) observeRepresentation(header http.Header) error {
	validator, strong := strongETag(header)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	switch r.representation {
	case readerAtRepresentationUnknown:
		if strong {
			r.representation = readerAtRepresentationPinned
			r.validator = validator
		} else {
			r.representation = readerAtRepresentationUnverifiable
		}
	case readerAtRepresentationPinned:
		if !strong {
			r.failure = fmt.Errorf("%w: pinned ETag %s, response omitted a strong ETag", errReaderAtRepresentationChanged, r.validator)
			return r.failure
		}
		if validator != r.validator {
			r.failure = fmt.Errorf("%w: pinned ETag %s, response ETag %s", errReaderAtRepresentationChanged, r.validator, validator)
			return r.failure
		}
	case readerAtRepresentationUnverifiable:
		// Weak or missing validators cannot prove a stable representation. Keep the
		// upstream usable without retroactively binding its blocks to a later ETag.
	}
	return nil
}

func strongETag(header http.Header) (string, bool) {
	values := header.Values("ETag")
	if len(values) != 1 {
		return "", false
	}
	validator := strings.TrimSpace(values[0])
	if len(validator) < 2 || validator[0] != '"' || validator[len(validator)-1] != '"' {
		return "", false
	}
	for i := 1; i < len(validator)-1; i++ {
		c := validator[i]
		if c == '"' || c < 0x21 || c == 0x7f {
			return "", false
		}
	}
	return validator, true
}

func (r *ReaderAt) cacheBlock(i int64, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if _, ok := r.blocks[i]; ok {
		return nil
	}
	r.blocks[i] = data
	r.order = append(r.order, i)
	if len(r.order) > r.maxBlocks {
		delete(r.blocks, r.order[0])
		r.order = r.order[1:]
	}
	return nil
}

// ReaderAtFor builds a ReaderAt over a storage file using its cached temporary link.
func (s *Streamer) ReaderAtFor(ctx context.Context, t vfs.Target, size int64) *ReaderAt {
	return NewReaderAt(ctx, s.HTTP, size,
		func(ctx context.Context) (string, error) { return s.TempLink(ctx, t) },
		func() { s.Cache.Delete(vfs.TempLinkKey(t.Storage.ID, t.DbxPath)) })
}
