// Package server wires HTTP routes, middleware and background jobs.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/backup"
	"github.com/dvfs/dropbox-vfs-manager/internal/cache"
	"github.com/dvfs/dropbox-vfs-manager/internal/config"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/limiter"
	"github.com/dvfs/dropbox-vfs-manager/internal/media"
	"github.com/dvfs/dropbox-vfs-manager/internal/pathutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
	"github.com/dvfs/dropbox-vfs-manager/internal/settings"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
	"github.com/dvfs/dropbox-vfs-manager/internal/stream"
	"github.com/dvfs/dropbox-vfs-manager/internal/thumb"
	"github.com/dvfs/dropbox-vfs-manager/internal/vfs"
	"github.com/dvfs/dropbox-vfs-manager/internal/web"
)

// Server holds every dependency.
type Server struct {
	Cfg      *config.Config
	DB       *sql.DB
	Box      *secret.Box
	Auth     *auth.Service
	Settings *settings.Store
	Cache    *cache.Cache
	St       *storage.Manager
	VFS      *vfs.Service
	Stream   *stream.Streamer
	Thumbs   *thumb.Service
	Prober   *media.Prober
	HLS      *media.Manager
	Backups  *backup.Service

	FolderSizeLim *limiter.Limiter
	SearchLim     *limiter.Limiter
	ZipLim        *limiter.Limiter

	logger *auditLogger

	// restoreMu makes a restore apply single-flight: a second apply gets 409 instead of racing.
	restoreMu sync.Mutex
	// restoreHook, when set, runs inside the restore critical section. Tests use it to hold the
	// lock long enough to observe a concurrent apply.
	restoreHook func()

	zipMu     sync.Mutex
	zipTokens map[string]zipJob

	SPA  fs.FS
	stop chan struct{}
	wg   sync.WaitGroup
}

type zipJob struct {
	paths   []string
	name    string
	expires time.Time
}

// New builds a server from config.
func New(cfg *config.Config, d *sql.DB) (*Server, error) {
	box, err := secret.New(cfg.Key)
	if err != nil {
		return nil, err
	}
	au, err := auth.New(d)
	if err != nil {
		return nil, err
	}
	ss, err := settings.Open(d)
	if err != nil {
		return nil, err
	}
	cur := ss.Get()
	c := cache.New(200_000, 256<<20, time.Duration(cur.CacheCleanupSec)*time.Second)
	sm, err := storage.NewManager(d, box, cfg)
	if err != nil {
		return nil, err
	}
	v := vfs.New(sm, c)
	th, err := thumb.New(sm, filepath.Join(cfg.DataDir, "thumbs"), func() int64 { return int64(ss.Get().ThumbCacheMB) << 20 })
	if err != nil {
		return nil, err
	}
	s := &Server{
		Cfg: cfg, DB: d, Box: box, Auth: au, Settings: ss, Cache: c, St: sm, VFS: v,
		Stream:        stream.New(sm, c, box),
		Thumbs:        th,
		Prober:        &media.Prober{DB: d, FFprobe: cfg.FFprobePath},
		HLS:           media.NewManager(cfg.FFmpegPath, filepath.Join(cfg.DataDir, "hls"), cur.TranscodeConcurrency),
		Backups:       backup.NewService(d, cfg.DataDir, box, cfg.Key),
		FolderSizeLim: limiter.New(cur.FolderSizeConcurrency),
		SearchLim:     limiter.New(cur.SearchConcurrency),
		ZipLim:        limiter.New(4),
		logger:        newAuditLogger(slog.Default()),
		zipTokens:     map[string]zipJob{},
		SPA:           web.FS(),
		stop:          make(chan struct{}),
	}
	ss.OnChange(func(old, n settings.Settings) {
		if old.CacheCleanupSec != n.CacheCleanupSec {
			c.SetCleanupInterval(time.Duration(n.CacheCleanupSec) * time.Second)
		}
		s.HLS.Limit.SetLimit(n.TranscodeConcurrency)
		s.FolderSizeLim.SetLimit(n.FolderSizeConcurrency)
		s.SearchLim.SetLimit(n.SearchConcurrency)
	})
	sm.OnChange(func(id int64) { v.FlushStorage(id) })
	s.wg.Add(1)
	go s.housekeeping()
	return s, nil
}

// Close stops background work.
func (s *Server) Close() {
	close(s.stop)
	s.wg.Wait()
	s.HLS.Close()
	s.Cache.Stop()
}

func (s *Server) housekeeping() {
	defer s.wg.Done()
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.Auth.PurgeExpired()
			_, _ = s.DB.Exec(`DELETE FROM oauth_states WHERE expires_at < ?`, time.Now().Unix())
			s.zipMu.Lock()
			for k, j := range s.zipTokens {
				if time.Now().After(j.expires) {
					delete(s.zipTokens, k)
				}
			}
			s.zipMu.Unlock()
		}
	}
}

// Handler builds the Gin engine.
func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), s.securityHeaders)
	if len(s.Cfg.TrustedProxies) > 0 {
		_ = r.SetTrustedProxies(s.Cfg.TrustedProxies)
	} else {
		_ = r.SetTrustedProxies(nil)
	}
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.HandleMethodNotAllowed = false

	r.GET("/robots.txt", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(200, "text/plain; charset=utf-8", []byte(s.Settings.Get().RobotsTxt))
	})

	api := r.Group("/api")
	api.Use(s.csrf)
	api.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	api.POST("/auth/login", s.login)
	api.POST("/auth/logout", s.logout)
	api.GET("/auth/me", s.me)
	api.GET("/oauth/callback", s.oauthCallback)
	api.GET("/s/raw/:exp/:sig/*path", s.signedRaw)
	api.HEAD("/s/raw/:exp/:sig/*path", s.signedRaw)

	a := api.Group("")
	a.Use(s.requireSession)
	a.GET("/config", s.clientConfig)
	a.GET("/mounts", s.mounts)
	a.GET("/fs/list", s.list)
	a.GET("/fs/stat", s.stat)
	a.GET("/fs/siblings", s.siblings)
	a.GET("/fs/raw", s.raw)
	a.HEAD("/fs/raw", s.raw)
	a.GET("/fs/link", s.link)
	a.GET("/fs/thumb", s.thumb)
	a.GET("/fs/text", s.text)
	a.GET("/fs/preview", s.officePreview)
	a.GET("/fs/archive", s.archiveList)
	a.GET("/fs/folder-size", s.folderSize)
	a.POST("/fs/rename", s.rename)
	a.POST("/fs/move", s.moveCopy(true))
	a.POST("/fs/copy", s.moveCopy(false))
	a.POST("/fs/delete", s.delete)
	a.POST("/fs/zip", s.zipStart)
	a.GET("/fs/zip/:token", s.zipDownload)
	a.GET("/search", s.search)
	a.GET("/media/probe", s.probe)
	a.POST("/media/hls", s.hlsOpen)
	a.GET("/media/hls/:id/:file", s.hlsFile)
	a.POST("/media/hls/:id/ping", s.hlsPing)
	a.DELETE("/media/hls/:id", s.hlsClose)

	ad := a.Group("/admin")
	ad.GET("/profile", s.getProfile)
	ad.PUT("/profile", s.putProfile)
	ad.GET("/settings", s.getSettings)
	ad.PUT("/settings", s.putSettings)
	ad.GET("/system", s.system)
	ad.POST("/cache/clear", s.clearCache)
	ad.GET("/oauth/info", s.oauthInfo)
	ad.POST("/oauth/paste", s.oauthPaste)
	ad.GET("/storages", s.adminStorages)
	ad.POST("/storages", s.createStorage)
	ad.POST("/storages/check-mount", s.checkMount)
	ad.GET("/storages/:id", s.getStorage)
	ad.PUT("/storages/:id", s.updateStorage)
	ad.DELETE("/storages/:id", s.deleteStorage)
	ad.POST("/storages/:id/reauth", s.reauthStorage)
	ad.POST("/storages/:id/code", s.storageCode)

	// Backup and restore. Every route is behind the admin session and the CSRF check above.
	ad.POST("/backup", s.exportBackup)
	ad.POST("/restore/preview", s.restorePreview)
	ad.POST("/restore/apply", s.restoreApply)
	ad.GET("/backups/snapshots", s.listSnapshots)
	ad.POST("/backups/snapshots/:id/preview", s.previewSnapshot)

	r.NoRoute(s.spa)
	return r
}

func (s *Server) securityHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	c.Next()
}

// csrf requires a custom header on state-changing API requests. Browsers cannot send it
// cross-site without a CORS preflight, which this server never approves.
func (s *Server) csrf(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		c.Next()
		return
	}
	if c.GetHeader(auth.CSRFHeader) != auth.CSRFValue {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "요청 출처를 확인할 수 없습니다. 페이지를 새로고침하세요"})
		return
	}
	c.Next()
}

func (s *Server) requireSession(c *gin.Context) {
	tok, _ := c.Cookie(auth.CookieName)
	if !s.Auth.Validate(tok) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "로그인이 필요합니다", "code": "auth_required"})
		return
	}
	c.Next()
}

func (s *Server) setSessionCookie(c *gin.Context, tok string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: auth.CookieName, Value: tok, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.Cfg.SecureCookies(), SameSite: http.SameSiteStrictMode,
	})
}

// spa serves embedded files, falling back to index.html for client routes.
func (s *Server) spa(c *gin.Context) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Status(http.StatusNotFound)
		return
	}
	p := c.Request.URL.Path
	if strings.HasPrefix(p, "/api/") {
		c.JSON(http.StatusNotFound, gin.H{"error": "없는 API입니다"})
		return
	}
	name := strings.TrimPrefix(path.Clean(p), "/")
	if name != "" && name != "index.html" {
		if st, err := fs.Stat(s.SPA, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				c.Header("Cache-Control", "public, max-age=3600")
			}
			c.FileFromFS(name, http.FS(s.SPA))
			return
		}
		if strings.HasPrefix(name, "assets/") {
			c.Status(http.StatusNotFound)
			return
		}
	}
	b, err := fs.ReadFile(s.SPA, "index.html")
	if err != nil {
		c.Data(503, "text/html; charset=utf-8", []byte(`<!doctype html><meta charset="utf-8"><title>Dropbox VFS</title>
<p>프론트엔드가 빌드되지 않았습니다. 저장소 루트에서 <code>scripts\run-local.ps1</code>을 실행하거나 frontend를 빌드한 뒤 backend/internal/web/dist에 복사하세요.</p>`))
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.Header("Content-Security-Policy", strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob: https: http:",
		"media-src 'self' blob: https: http:",
		"connect-src 'self' https: http:",
		"worker-src 'self' blob:",
		"frame-src 'self' blob:",
		"font-src 'self' data:",
		"object-src 'none'",
		"base-uri 'self'",
		"frame-ancestors 'self'",
	}, "; "))
	c.Data(200, "text/html; charset=utf-8", b)
}

// apiError carries an HTTP status and user-facing message.
type apiError struct {
	status int
	msg    string
	field  string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(msg string) error { return &apiError{status: 400, msg: msg} }

// fail maps errors to responses with messages that say what happened and what to do.
func fail(c *gin.Context, err error) {
	var ae *apiError
	var ve *storage.ValidationError
	var de *dropbox.APIError
	var pe *pathutil.PathError
	switch {
	case errors.As(err, &pe):
		c.JSON(400, gin.H{"error": pe.Msg})
	case errors.As(err, &ae):
		c.JSON(ae.status, gin.H{"error": ae.msg, "field": ae.field})
	case errors.As(err, &ve):
		c.JSON(400, gin.H{"error": ve.Msg, "field": ve.Field})
	case errors.Is(err, vfs.ErrNotFound):
		c.JSON(404, gin.H{"error": "파일이나 폴더를 찾을 수 없습니다. 이동되었거나 삭제되었을 수 있습니다"})
	case errors.Is(err, storage.ErrState):
		c.JSON(400, gin.H{"error": err.Error()})
	case errors.Is(err, storage.ErrRestoreInProgress):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "restore_in_progress"})
	case errors.Is(err, dropbox.ErrReauth):
		c.JSON(409, gin.H{"error": err.Error(), "code": "reauth"})
	case errors.Is(err, media.ErrBusy):
		c.JSON(503, gin.H{"error": err.Error(), "code": "busy"})
	case errors.Is(err, context.Canceled):
		c.Status(499)
	case errors.As(err, &de):
		c.JSON(dropboxStatus(de), gin.H{"error": DropboxMessage(de), "detail": de.Summary})
	default:
		slog.Error("request failed", "path", c.Request.URL.Path, "err", err)
		c.JSON(500, gin.H{"error": err.Error()})
	}
}

func dropboxStatus(e *dropbox.APIError) int {
	switch e.Status {
	case 409:
		if strings.Contains(e.Summary, "not_found") {
			return 404
		}
		return 409
	case 429:
		return 429
	case 400, 401, 403:
		return 502
	}
	return 502
}

// DropboxMessage translates common Dropbox error summaries.
func DropboxMessage(e *dropbox.APIError) string {
	s := e.Summary
	switch {
	case strings.Contains(s, "conflict"):
		return "같은 이름의 항목이 이미 있습니다. 다른 이름을 쓰세요"
	case strings.Contains(s, "not_found"):
		return "Dropbox에서 항목을 찾을 수 없습니다. 목록을 새로고침하세요"
	case strings.Contains(s, "too_many_write_operations"):
		return "Dropbox에 쓰기 작업이 몰려 있습니다. 잠시 후 다시 시도하세요"
	case strings.Contains(s, "too_many_requests") || e.Status == 429:
		return "Dropbox 요청 한도에 걸렸습니다. 잠시 후 다시 시도하세요"
	case strings.Contains(s, "insufficient_space"):
		return "Dropbox 저장 공간이 부족합니다"
	case strings.Contains(s, "cant_move_folder_into_itself"), strings.Contains(s, "cant_nest_shared_folder"):
		return "폴더를 자기 자신 안으로 옮길 수 없습니다"
	case strings.Contains(s, "missing_scope"):
		return "Dropbox 앱 권한이 부족합니다. App Console에서 권한(scope)을 켜고 다시 인증하세요"
	case strings.Contains(s, "unsupported_extension"), strings.Contains(s, "unsupported_content"):
		return "Dropbox가 이 파일의 미리보기를 지원하지 않습니다. 다운로드해서 여세요"
	case strings.Contains(s, "in_progress"):
		return "Dropbox가 미리보기를 만드는 중입니다. 잠시 후 다시 여세요"
	case strings.Contains(s, "email_not_verified"):
		return "Dropbox 계정 이메일 인증이 필요합니다"
	}
	if s == "" {
		return fmt.Sprintf("Dropbox 요청이 실패했습니다 (HTTP %d)", e.Status)
	}
	return "Dropbox 요청이 실패했습니다: " + s
}

func bindJSON(c *gin.Context, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return badRequest("요청 형식이 올바르지 않습니다")
	}
	return nil
}

// baseURL returns public_url or the scheme+host the request came in on.
func (s *Server) baseURL(c *gin.Context) string {
	if s.Cfg.PublicURL != "" {
		return s.Cfg.PublicURL
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	} else if len(s.Cfg.TrustedProxies) > 0 {
		if p := c.GetHeader("X-Forwarded-Proto"); p == "https" {
			scheme = "https"
		}
	}
	return scheme + "://" + c.Request.Host
}
