package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dvfs/dropbox-vfs-manager/internal/auth"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

func (s *Server) login(c *gin.Context) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	tok, err := s.Auth.Login(in.Username, in.Password, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrLocked) {
			status = http.StatusTooManyRequests
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	s.setSessionCookie(c, tok, int(auth.SessionTTL.Seconds()))
	s.me2(c)
}

func (s *Server) logout(c *gin.Context) {
	tok, _ := c.Cookie(auth.CookieName)
	s.Auth.Logout(tok)
	s.setSessionCookie(c, "", -1)
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) me(c *gin.Context) {
	tok, _ := c.Cookie(auth.CookieName)
	if !s.Auth.Validate(tok) {
		c.JSON(200, gin.H{"authenticated": false})
		return
	}
	s.me2(c)
}

func (s *Server) me2(c *gin.Context) {
	a, err := s.Auth.Admin()
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"authenticated": true, "username": a.Username, "is_default_password": a.IsDefault})
}

func (s *Server) getProfile(c *gin.Context) {
	a, err := s.Auth.Admin()
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, a)
}

func (s *Server) putProfile(c *gin.Context) {
	var in struct {
		Username        string `json:"username"`
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	tok, _ := c.Cookie(auth.CookieName)
	nt, err := s.Auth.UpdateProfile(tok, in.CurrentPassword, in.Username, in.NewPassword, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if nt != tok {
		s.setSessionCookie(c, nt, int(auth.SessionTTL.Seconds()))
	}
	a, _ := s.Auth.Admin()
	c.JSON(200, a)
}

func (s *Server) getSettings(c *gin.Context) { c.JSON(200, s.Settings.Get()) }

func (s *Server) putSettings(c *gin.Context) {
	n := s.Settings.Get()
	if err := bindJSON(c, &n); err != nil {
		fail(c, err)
		return
	}
	if err := s.Settings.Update(n); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, s.Settings.Get())
}

// clientConfig is what every page needs to render.
func (s *Server) clientConfig(c *gin.Context) {
	st := s.Settings.Get()
	c.JSON(200, gin.H{
		"pagination_type":  st.PaginationType,
		"per_page":         st.PerPage,
		"audio_autoplay":   st.AudioAutoplay,
		"video_autoplay":   st.VideoAutoplay,
		"archive_preview":  st.ArchivePreview,
		"data_mode":        st.DataMode,
		"public_url":       s.Cfg.PublicURL,
		"transcode":        s.HLS.Available() && st.TranscodeConcurrency > 0,
		"probe":            s.Prober.Available(),
	})
}

func (s *Server) system(c *gin.Context) {
	files, bytes := s.Thumbs.Usage()
	fa, fw, fl := s.FolderSizeLim.Stats()
	ha, hw, hl := s.HLS.Limit.Stats()
	c.JSON(200, gin.H{
		"cache":       s.Cache.Stats(),
		"thumbs":      gin.H{"files": files, "bytes": bytes, "max_bytes": int64(s.Settings.Get().ThumbCacheMB) << 20},
		"folder_size": gin.H{"active": fa, "waiting": fw, "limit": fl},
		"transcode":   gin.H{"active": ha, "waiting": hw, "limit": hl, "sessions": s.HLS.Count()},
		"ffmpeg":      s.Cfg.FFmpegPath,
		"ffprobe":     s.Cfg.FFprobePath,
		"storages":    len(s.St.All()),
		"data_dir":    s.Cfg.DataDir,
	})
}

func (s *Server) clearCache(c *gin.Context) {
	var in struct {
		Thumbs bool `json:"thumbs"`
	}
	_ = bindJSON(c, &in)
	s.Cache.Clear()
	if in.Thumbs {
		s.Thumbs.Clear()
	}
	c.JSON(200, gin.H{"ok": true})
}

// ---- storages ----

func (s *Server) oauthInfo(c *gin.Context) {
	c.JSON(200, gin.H{
		"localhost_redirect": s.Cfg.LocalhostCallback(),
		"public_redirect":    s.Cfg.PublicCallback(),
		"public_url":         s.Cfg.PublicURL,
		"scopes":             []string{"account_info.read", "files.metadata.read", "files.content.read", "files.content.write"},
	})
}

func (s *Server) adminStorages(c *gin.Context) { c.JSON(200, s.St.All()) }

func idParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "잘못된 저장소 번호입니다"})
		return 0, false
	}
	return id, true
}

func (s *Server) getStorage(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	st, found := s.St.Get(id)
	if !found {
		c.JSON(404, gin.H{"error": "저장소를 찾을 수 없습니다"})
		return
	}
	c.JSON(200, st)
}

func (s *Server) createStorage(c *gin.Context) {
	var in storage.Input
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	res, err := s.St.Create(in)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(201, res)
}

func (s *Server) checkMount(c *gin.Context) {
	var in struct {
		MountPath string `json:"mount_path"`
		ID        int64  `json:"id"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	if err := s.St.CheckMount(in.MountPath, in.ID); err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) updateStorage(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in storage.Input
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	st, err := s.St.Update(id, in)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, st)
}

func (s *Server) deleteStorage(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := s.St.Delete(c.Request.Context(), id); err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) reauthStorage(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		AuthMode string `json:"auth_mode"`
	}
	_ = bindJSON(c, &in)
	res, err := s.St.Reauth(id, in.AuthMode)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, res)
}

func (s *Server) storageCode(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	st, err := s.St.CompleteWithCode(ctx, id, in.Code)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, st)
}

func (s *Server) oauthPaste(c *gin.Context) {
	var in struct {
		URL string `json:"url"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	code, state, err := storage.ParsePastedURL(in.URL)
	if err != nil {
		fail(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	st, err := s.St.CompleteWithState(ctx, state, code)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, st)
}

// oauthCallback finishes a redirect-mode authorization. It needs no session: the one-time
// state proves the request belongs to an authorization a logged-in admin started.
func (s *Server) oauthCallback(c *gin.Context) {
	page := func(status int, title, body string) {
		c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		c.Header("Cache-Control", "no-store")
		c.Data(status, "text/html; charset=utf-8", []byte(fmt.Sprintf(`<!doctype html><html lang="ko"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>body{font:16px/1.6 system-ui,sans-serif;background:#14171c;color:#e6e8eb;display:grid;place-items:center;min-height:100vh;margin:0}
main{max-width:34rem;padding:2rem}h1{font-size:1.4rem;margin:0 0 .5rem}p{color:#a9b0ba}</style></head>
<body><main><h1>%s</h1><p>%s</p></main></body></html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))))
	}
	q := c.Request.URL.Query()
	if e := q.Get("error"); e != "" {
		page(400, "Dropbox 연결이 취소되었습니다", "Dropbox가 '"+e+"' 응답을 보냈습니다. 관리자 저장소 화면에서 다시 인증을 시작하세요.")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	st, err := s.St.CompleteWithState(ctx, q.Get("state"), q.Get("code"))
	if err != nil {
		page(400, "Dropbox 연결을 마치지 못했습니다", err.Error())
		return
	}
	who := st.AccountEmail
	if who == "" {
		who = st.AccountName
	}
	page(200, "연결 완료, 원래 탭으로 돌아가세요", who+" 계정이 "+st.MountPath+" 에 연결되었습니다. 이 탭은 닫아도 됩니다.")
}
