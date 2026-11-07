package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"github.com/dvfs/dropbox-vfs-manager/internal/archive"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/pathutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/stream"
	"github.com/dvfs/dropbox-vfs-manager/internal/thumb"
	"github.com/dvfs/dropbox-vfs-manager/internal/vfs"
	"github.com/dvfs/dropbox-vfs-manager/internal/zipstream"
)

// mountInfo is the public view of a storage for the browser.
type mountInfo struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	MountPath    string `json:"mount_path"`
	Status       string `json:"status"`
	AccountEmail string `json:"account_email"`
	AccountName  string `json:"account_name"`
}

func (s *Server) mounts(c *gin.Context) {
	all := s.St.All()
	out := make([]mountInfo, len(all))
	for i, st := range all {
		out[i] = mountInfo{ID: st.ID, Name: st.Name, MountPath: st.MountPath, Status: st.Status, AccountEmail: st.AccountEmail, AccountName: st.AccountName}
	}
	c.JSON(200, out)
}

func (s *Server) list(c *gin.Context) {
	off, _ := strconv.Atoi(c.Query("offset"))
	lim, _ := strconv.Atoi(c.Query("limit"))
	if lim < 0 || lim > 5000 {
		lim = 5000
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	res, err := s.VFS.List(ctx, c.DefaultQuery("path", "/"), vfs.ListOptions{Sort: c.Query("sort"), Order: c.Query("order"), Offset: off, Limit: lim})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, res)
}

func (s *Server) stat(c *gin.Context) {
	e, _, err := s.VFS.Stat(c.Request.Context(), c.Query("path"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, e)
}

func (s *Server) siblings(c *gin.Context) {
	es, err := s.VFS.SiblingFiles(c.Request.Context(), c.Query("path"), c.Query("sort"), c.Query("order"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, es)
}

// fileTarget resolves a path that must be a file.
func (s *Server) fileTarget(c *gin.Context, vpath string) (vfs.Entry, vfs.Target, bool) {
	e, t, err := s.VFS.Stat(c.Request.Context(), vpath)
	if err != nil {
		fail(c, err)
		return e, t, false
	}
	if e.Type != vfs.TypeFile {
		fail(c, badRequest("파일이 아니라 폴더입니다"))
		return e, t, false
	}
	return e, t, true
}

// raw serves file content. Direct mode redirects to a Dropbox temporary link unless
// proxy=1 forces streaming through the server (needed for same-origin PDF, VR textures).
func (s *Server) raw(c *gin.Context) {
	e, t, ok := s.fileTarget(c, c.Query("path"))
	if !ok {
		return
	}
	download := c.Query("download") == "1"
	if tok := c.Query("fresh"); tok != "" && len(tok) <= 32 {
		// The player's previous link expired mid-playback: get a new one from Dropbox, once
		// per token, since the media element keeps requesting the same URL for every range.
		mark := vfs.TempLinkKey(t.Storage.ID, t.DbxPath) + "|fresh|" + tok
		if _, seen := s.Cache.Get(mark); !seen {
			s.Cache.Delete(vfs.TempLinkKey(t.Storage.ID, t.DbxPath))
			s.Cache.Set(mark, true, 8, time.Hour)
		}
	}
	if s.Settings.Get().DataMode == settings.ModeDirect && c.Query("proxy") != "1" {
		link, err := s.Stream.TempLink(c.Request.Context(), t)
		if err != nil {
			fail(c, err)
			return
		}
		c.Header("Cache-Control", "private, max-age=600")
		c.Redirect(http.StatusFound, link)
		return
	}
	if err := s.Stream.Proxy(c.Writer, c.Request, t, e.Name, download); err != nil && !c.Writer.Written() {
		fail(c, err)
	}
}

func (s *Server) signedRaw(c *gin.Context) {
	vp, err := pathutil.Clean(c.Param("path"))
	if err != nil || !s.Stream.VerifySigned(vp, c.Param("exp"), c.Param("sig")) {
		c.JSON(403, gin.H{"error": "링크가 만료되었거나 올바르지 않습니다. 사이트에서 다시 여세요"})
		return
	}
	e, t, ok := s.fileTarget(c, vp)
	if !ok {
		return
	}
	if err := s.Stream.Proxy(c.Writer, c.Request, t, e.Name, false); err != nil && !c.Writer.Written() {
		fail(c, err)
	}
}

// link returns a URL an external player (PotPlayer) can open without the site cookie.
func (s *Server) link(c *gin.Context) {
	_, t, ok := s.fileTarget(c, c.Query("path"))
	if !ok {
		return
	}
	if s.Settings.Get().DataMode == settings.ModeDirect {
		l, err := s.Stream.TempLink(c.Request.Context(), t)
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"url": l, "kind": "direct"})
		return
	}
	c.JSON(200, gin.H{"url": s.baseURL(c) + s.Stream.SignedPath(t.VPath, 12*time.Hour), "kind": "signed"})
}

func (s *Server) thumb(c *gin.Context) {
	t, err := s.VFS.Resolve(c.Query("path"))
	if err != nil {
		fail(c, err)
		return
	}
	if t.Virtual || !thumb.Thumbnailable(t.VPath) {
		c.Status(404)
		return
	}
	rev := c.Query("rev")
	etag := `"` + rev + "-" + c.Query("size") + `"`
	if rev != "" && c.GetHeader("If-None-Match") == etag {
		c.Status(304)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	b, err := s.Thumbs.Get(ctx, t.Storage.ID, t.DbxPath, rev, c.Query("size"))
	if err != nil {
		if errors.Is(err, thumb.ErrNoThumb) {
			c.Status(404)
			return
		}
		fail(c, err)
		return
	}
	c.Header("Cache-Control", "private, max-age=604800")
	if rev != "" {
		c.Header("ETag", etag)
	}
	c.Data(200, "image/jpeg", b)
}

// TextLimit is the maximum bytes returned for a text preview.
const TextLimit = 1 << 20

func decodeText(b []byte) (string, string) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:]), "utf-8"
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}), bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		out, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder(), b)
		if err == nil {
			return string(out), "utf-16"
		}
	}
	// A truncated multi-byte sequence at the end is fine; trim it before checking.
	trim := b
	for i := 0; i < 3 && len(trim) > 0 && !utf8.Valid(trim); i++ {
		trim = trim[:len(trim)-1]
	}
	if utf8.Valid(trim) {
		return string(trim), "utf-8"
	}
	out, _, err := transform.Bytes(korean.EUCKR.NewDecoder(), b)
	if err == nil {
		return string(out), "euc-kr"
	}
	return string(bytes.ToValidUTF8(b, []byte("\uFFFD"))), "unknown"
}

func (s *Server) text(c *gin.Context) {
	e, t, ok := s.fileTarget(c, c.Query("path"))
	if !ok {
		return
	}
	cl, err := s.St.Client(t.Storage.ID)
	if err != nil {
		fail(c, err)
		return
	}
	h := http.Header{}
	h.Set("Range", fmt.Sprintf("bytes=0-%d", TextLimit-1))
	resp, err := cl.Download(c.Request.Context(), t.DbxPath, h)
	if err != nil {
		fail(c, err)
		return
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, TextLimit))
	if err != nil {
		fail(c, err)
		return
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 && !bytes.HasPrefix(b, []byte{0xFF, 0xFE}) && !bytes.HasPrefix(b, []byte{0xFE, 0xFF}) {
		c.JSON(415, gin.H{"error": "텍스트가 아닌 파일입니다. 다운로드해서 여세요"})
		return
	}
	txt, enc := decodeText(b)
	c.JSON(200, gin.H{"text": txt, "encoding": enc, "truncated": *e.Size > int64(len(b)), "size": *e.Size})
}

func (s *Server) officePreview(c *gin.Context) {
	_, t, ok := s.fileTarget(c, c.Query("path"))
	if !ok {
		return
	}
	cl, err := s.St.Client(t.Storage.ID)
	if err != nil {
		fail(c, err)
		return
	}
	resp, err := cl.GetPreview(c.Request.Context(), t.DbxPath)
	if err != nil {
		fail(c, err)
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/pdf"
	}
	c.Header("Cache-Control", "private, max-age=600")
	c.Header("Content-Disposition", "inline")
	if strings.HasPrefix(ct, "text/html") {
		// Spreadsheet previews are Dropbox-generated HTML; render it without scripts.
		c.Header("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:")
	}
	c.DataFromReader(200, resp.ContentLength, ct, resp.Body, nil)
}

func (s *Server) archiveList(c *gin.Context) {
	st := s.Settings.Get()
	if !st.ArchivePreview {
		c.JSON(403, gin.H{"error": "관리자가 압축 파일 미리보기를 껐습니다. 다운로드해서 여세요"})
		return
	}
	e, t, ok := s.fileTarget(c, c.Query("path"))
	if !ok {
		return
	}
	format := archive.Format(e.Name)
	if format == "" {
		fail(c, badRequest("지원하지 않는 압축 형식입니다"))
		return
	}
	key := fmt.Sprintf("s:%d:arc:%s:%s", t.Storage.ID, strings.ToLower(t.DbxPath), e.Rev)
	v, err := s.Cache.GetOrLoad(key, func() (any, int64, time.Duration, error) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
		defer cancel()
		ra := s.Stream.ReaderAtFor(ctx, t, *e.Size)
		l, err := archive.List(format, ra, *e.Size, int64(st.ArchiveMaxMB)<<20)
		if err != nil {
			return nil, 0, 0, err
		}
		b, _ := json.Marshal(l)
		return l, int64(len(b)), t.Storage.CacheTTL(), nil
	})
	if err != nil {
		fail(c, badRequest(err.Error()))
		return
	}
	c.JSON(200, gin.H{"listing": v, "sequential": archive.Sequential(format), "budget_mb": st.ArchiveMaxMB})
}

// sse prepares a Server-Sent Events response and returns a send function.
func sse(c *gin.Context) func(event string, v any) bool {
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	c.Status(200)
	c.Writer.Flush()
	return func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		c.Writer.Flush()
		return true
	}
}

func (s *Server) folderSize(c *gin.Context) {
	ctx := c.Request.Context()
	send := sse(c)
	type msg struct {
		event string
		v     any
	}
	ch := make(chan msg, 16)
	go func() {
		defer close(ch)
		p, err := s.VFS.FolderSize(ctx, c.Query("path"), s.FolderSizeLim, func(p vfs.SizeProgress) {
			select {
			case ch <- msg{"progress", p}:
			default: // drop progress if the client is slow
			}
		})
		if err != nil {
			if ctx.Err() == nil {
				ch <- msg{"error", gin.H{"error": errorText(err)}}
			}
			return
		}
		ch <- msg{"done", p}
	}()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return
			}
			if !send(m.event, m.v) {
				return
			}
		case <-tick.C:
			fmt.Fprint(c.Writer, ": ping\n\n")
			c.Writer.Flush()
		case <-ctx.Done():
			return
		}
	}
}

func errorText(err error) string {
	var de *dropbox.APIError
	if errors.As(err, &de) {
		return DropboxMessage(de)
	}
	if errors.Is(err, vfs.ErrNotFound) {
		return "폴더를 찾을 수 없습니다"
	}
	return err.Error()
}

// opTarget resolves a path that must be a real file or folder (not a mount or virtual folder).
func (s *Server) opTarget(ctx context.Context, vp string) (vfs.Entry, vfs.Target, error) {
	e, t, err := s.VFS.Stat(ctx, vp)
	if err != nil {
		return e, t, fmt.Errorf("%s: %w", vp, err)
	}
	if e.Type == vfs.TypeMount || e.Type == vfs.TypeVirtual {
		return e, t, badRequest("저장소 마운트 폴더나 가상 폴더는 바꿀 수 없습니다. 관리자 저장소 메뉴를 쓰세요")
	}
	if t.Storage.Status != storage.StatusActive {
		return e, t, &apiError{status: 409, msg: "이 저장소는 인증이 필요합니다"}
	}
	return e, t, nil
}

func (s *Server) rename(c *gin.Context) {
	var in struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	if err := pathutil.ValidName(in.Name); err != nil {
		fail(c, badRequest(err.Error()))
		return
	}
	_, t, err := s.opTarget(c.Request.Context(), in.Path)
	if err != nil {
		fail(c, err)
		return
	}
	to := path.Join(pathutil.Parent(t.DbxPath), in.Name)
	cl, err := s.St.Client(t.Storage.ID)
	if err != nil {
		fail(c, err)
		return
	}
	if err := cl.Move(c.Request.Context(), t.DbxPath, to); err != nil {
		fail(c, err)
		return
	}
	s.VFS.InvalidatePath(t.Storage.ID, t.DbxPath)
	s.VFS.InvalidatePath(t.Storage.ID, to)
	c.JSON(200, gin.H{"ok": true, "path": path.Join(pathutil.Parent(t.VPath), in.Name)})
}

type opFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

func (s *Server) moveCopy(move bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			Paths []string `json:"paths"`
			Dest  string   `json:"dest"`
		}
		if err := bindJSON(c, &in); err != nil {
			fail(c, err)
			return
		}
		if len(in.Paths) == 0 || len(in.Paths) > 1000 {
			fail(c, badRequest("항목을 1~1000개 선택하세요"))
			return
		}
		ctx := c.Request.Context()
		de, dt, err := s.VFS.Stat(ctx, in.Dest)
		if err != nil {
			fail(c, err)
			return
		}
		if de.Type == vfs.TypeFile || dt.Virtual {
			fail(c, badRequest("대상은 저장소 안의 폴더여야 합니다"))
			return
		}
		var entries []dropbox.RelocationEntry
		var vpaths []string
		for _, p := range in.Paths {
			_, t, err := s.opTarget(ctx, p)
			if err != nil {
				fail(c, err)
				return
			}
			if t.Storage.ID != dt.Storage.ID {
				fail(c, badRequest("이동과 복사는 같은 저장소 안에서만 할 수 있습니다"))
				return
			}
			if pathutil.HasPrefix(dt.DbxPath, t.DbxPath) {
				fail(c, badRequest("폴더를 자기 자신이나 하위 폴더로 옮길 수 없습니다"))
				return
			}
			if strings.EqualFold(pathutil.Parent(t.DbxPath), dt.DbxPath) && move {
				fail(c, badRequest("이미 그 폴더에 있습니다"))
				return
			}
			entries = append(entries, dropbox.RelocationEntry{FromPath: t.DbxPath, ToPath: path.Join(dt.DbxPath, path.Base(t.DbxPath))})
			vpaths = append(vpaths, p)
		}
		cl, err := s.St.Client(dt.Storage.ID)
		if err != nil {
			fail(c, err)
			return
		}
		var fails []opFailure
		if len(entries) == 1 {
			if move {
				err = cl.Move(ctx, entries[0].FromPath, entries[0].ToPath)
			} else {
				err = cl.Copy(ctx, entries[0].FromPath, entries[0].ToPath)
			}
			if err != nil {
				fail(c, err)
				return
			}
		} else {
			var bf []dropbox.BatchFailure
			if move {
				bf, err = cl.MoveBatch(ctx, entries)
			} else {
				bf, err = cl.CopyBatch(ctx, entries)
			}
			if err != nil {
				fail(c, err)
				return
			}
			for _, f := range bf {
				fails = append(fails, opFailure{Path: vpaths[f.Index], Error: DropboxMessage(&dropbox.APIError{Summary: f.Summary})})
			}
		}
		for _, e := range entries {
			if move {
				s.VFS.InvalidatePath(dt.Storage.ID, e.FromPath)
			}
			s.VFS.InvalidatePath(dt.Storage.ID, e.ToPath)
		}
		c.JSON(200, gin.H{"ok": len(fails) == 0, "failures": fails, "done": len(entries) - len(fails)})
	}
}

func (s *Server) delete(c *gin.Context) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	if len(in.Paths) == 0 || len(in.Paths) > 1000 {
		fail(c, badRequest("항목을 1~1000개 선택하세요"))
		return
	}
	ctx := c.Request.Context()
	type group struct {
		dbx    []string
		vpaths []string
	}
	groups := map[int64]*group{}
	for _, p := range in.Paths {
		_, t, err := s.opTarget(ctx, p)
		if err != nil {
			fail(c, err)
			return
		}
		g := groups[t.Storage.ID]
		if g == nil {
			g = &group{}
			groups[t.Storage.ID] = g
		}
		g.dbx = append(g.dbx, t.DbxPath)
		g.vpaths = append(g.vpaths, p)
	}
	var fails []opFailure
	done := 0
	for id, g := range groups {
		cl, err := s.St.Client(id)
		if err != nil {
			for _, p := range g.vpaths {
				fails = append(fails, opFailure{p, err.Error()})
			}
			continue
		}
		if len(g.dbx) == 1 {
			if err := cl.Delete(ctx, g.dbx[0]); err != nil {
				fails = append(fails, opFailure{g.vpaths[0], errorText(err)})
			} else {
				done++
			}
		} else {
			bf, err := cl.DeleteBatch(ctx, g.dbx)
			if err != nil {
				for _, p := range g.vpaths {
					fails = append(fails, opFailure{p, errorText(err)})
				}
			} else {
				for _, f := range bf {
					fails = append(fails, opFailure{g.vpaths[f.Index], DropboxMessage(&dropbox.APIError{Summary: f.Summary})})
				}
				done += len(g.dbx) - len(bf)
			}
		}
		for _, d := range g.dbx {
			s.VFS.InvalidatePath(id, d)
		}
	}
	c.JSON(200, gin.H{"ok": len(fails) == 0, "failures": fails, "done": done})
}

func (s *Server) zipStart(c *gin.Context) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	if len(in.Paths) == 0 || len(in.Paths) > 5000 {
		fail(c, badRequest("항목을 1~5000개 선택하세요"))
		return
	}
	for i, p := range in.Paths {
		cp, err := pathutil.Clean(p)
		if err != nil {
			fail(c, badRequest(err.Error()))
			return
		}
		in.Paths[i] = cp
	}
	name := "download-" + time.Now().Format("20060102-150405")
	if len(in.Paths) == 1 {
		if b := path.Base(in.Paths[0]); b != "/" {
			name = b
		}
	}
	tok, err := secret.RandomToken(18)
	if err != nil {
		fail(c, err)
		return
	}
	s.zipMu.Lock()
	s.zipTokens[tok] = zipJob{paths: in.Paths, name: name, expires: time.Now().Add(10 * time.Minute)}
	s.zipMu.Unlock()
	c.JSON(200, gin.H{"url": "/api/fs/zip/" + tok, "name": name + ".zip"})
}

func (s *Server) zipDownload(c *gin.Context) {
	tok := c.Param("token")
	// Token validity and admission are decided under one lock so the three answers this endpoint
	// owes the client stay separate: an unknown or expired token is always 404, a busy limiter is
	// 503 with the token kept, and a slot plus a valid token consumes the token exactly once.
	s.zipMu.Lock()
	job, ok := s.zipTokens[tok]
	if !ok || time.Now().After(job.expires) {
		delete(s.zipTokens, tok)
		s.zipMu.Unlock()
		c.JSON(404, gin.H{"error": "압축 다운로드 링크가 만료되었습니다. 다시 선택해서 받으세요"})
		return
	}
	if !s.ZipLim.TryAcquire() {
		s.zipMu.Unlock()
		c.JSON(503, gin.H{"error": "동시에 진행 중인 압축 다운로드가 많습니다. 잠시 후 다시 시도하세요"})
		return
	}
	delete(s.zipTokens, tok)
	s.zipMu.Unlock()
	ctx := c.Request.Context()
	defer s.ZipLim.Release()
	files, err := zipstream.Expand(ctx, s.VFS, job.paths, 100000)
	if err != nil {
		fail(c, err)
		return
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", stream.ContentDisposition("attachment", job.name+".zip"))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Estimated-Size", strconv.FormatInt(zipstream.TotalSize(files), 10))
	c.Status(200)
	open := func(ctx context.Context, id int64, dbx string) (io.ReadCloser, error) {
		cl, err := s.St.Client(id)
		if err != nil {
			return nil, err
		}
		resp, err := cl.Download(ctx, dbx, nil)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	if err := zipstream.Write(ctx, c.Writer, files, open); err != nil && ctx.Err() == nil {
		// Headers are already sent; the truncated archive will fail its integrity check.
		c.Error(err)
	}
}
