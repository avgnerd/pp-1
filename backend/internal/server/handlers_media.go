package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dvfs/dropbox-vfs-manager/internal/media"
	"github.com/dvfs/dropbox-vfs-manager/internal/search"
)

func (s *Server) search(c *gin.Context) {
	kind := c.DefaultQuery("kind", search.KindAll)
	if kind != search.KindAll && kind != search.KindFile && kind != search.KindFolder {
		kind = search.KindAll
	}
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		fail(c, badRequest("검색어를 입력하세요"))
		return
	}
	ctx := c.Request.Context()
	send := sse(c)
	events := make(chan search.Event, 32)
	errc := make(chan error, 1)
	go func() {
		errc <- search.Run(ctx, s.St, s.SearchLim, search.Options{Query: q, Kind: kind, Scope: c.Query("scope")}, func(e search.Event) {
			select {
			case events <- e:
			case <-ctx.Done():
			}
		})
		close(events)
	}()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-events:
			if !ok {
				if err := <-errc; err != nil && ctx.Err() == nil {
					send("error", gin.H{"error": err.Error()})
				}
				return
			}
			if !send(e.Type, e) {
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

func (s *Server) probe(c *gin.Context) {
	e, t, ok := s.fileTarget(c, c.Query("path"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	var link string
	if s.Prober.Available() {
		l, err := s.Stream.TempLink(ctx, t)
		if err != nil {
			fail(c, err)
			return
		}
		link = l
	}
	pr, err := s.Prober.Probe(ctx, e.ID, e.Rev, e.Name, link)
	if err != nil {
		// Probing failure should not block original playback.
		c.JSON(200, gin.H{"probe": media.Probe{VR: media.DetectVRName(e.Name)}, "probe_error": err.Error(), "transcode": s.HLS.Available()})
		return
	}
	c.JSON(200, gin.H{"probe": pr, "transcode": s.HLS.Available() && s.Settings.Get().TranscodeConcurrency > 0})
}

func (s *Server) hlsOpen(c *gin.Context) {
	var in struct {
		Path   string `json:"path"`
		Mode   string `json:"mode"`
		Height int    `json:"height"`
		Client string `json:"client"`
	}
	if err := bindJSON(c, &in); err != nil {
		fail(c, err)
		return
	}
	if len(in.Client) > 64 {
		in.Client = in.Client[:64]
	}
	switch in.Mode {
	case media.ModeRemux, media.ModeTranscode, media.ModeAudio:
	default:
		fail(c, badRequest("mode는 remux, transcode, audio 중 하나여야 합니다"))
		return
	}
	if in.Height <= 0 || in.Height > 2160 {
		in.Height = 1080
	}
	e, t, ok := s.fileTarget(c, in.Path)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	link, err := s.Stream.TempLink(ctx, t)
	if err != nil {
		fail(c, err)
		return
	}
	pr, err := s.Prober.Probe(ctx, e.ID, e.Rev, e.Name, link)
	if err != nil {
		fail(c, badRequest("파일 정보를 읽지 못해 변환할 수 없습니다: "+err.Error()))
		return
	}
	if in.Mode != media.ModeAudio && pr.Video == nil {
		in.Mode = media.ModeAudio
	}
	srcH := 0
	end := pr.Duration
	if pr.Video != nil {
		srcH = pr.Video.Height
		// ffmpeg cuts segments on video packets, so audio running past the last frame never gets
		// segments of its own; end the playlist with the picture.
		if in.Mode != media.ModeAudio && pr.Video.End > 0 && pr.Video.End < end {
			end = pr.Video.End
		}
	}
	tt := t
	sess, err := s.HLS.Open(media.Source{
		Key:      fmt.Sprintf("%d|%s|%s", t.Storage.ID, e.ID, e.Rev),
		Client:   in.Client,
		URL:      func(ctx context.Context) (string, error) { return s.Stream.TempLink(ctx, tt) },
		Duration: end, Mode: in.Mode, Height: in.Height, SrcH: srcH,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"id": sess.ID, "playlist": "/api/media/hls/" + sess.ID + "/index.m3u8", "mode": in.Mode, "duration": end})
}

func (s *Server) hlsFile(c *gin.Context) {
	sess, ok := s.HLS.Get(c.Param("id"))
	if !ok {
		// 410, not 404: the player must not retry this URL but open a new session.
		c.JSON(410, gin.H{"error": "변환 세션이 끝났습니다. 영상을 다시 여세요", "code": "session_gone"})
		return
	}
	f := c.Param("file")
	c.Header("Cache-Control", "no-cache")
	switch {
	case f == "index.m3u8":
		c.Data(200, "application/vnd.apple.mpegurl", []byte(sess.Playlist()))
	case f == "init.mp4":
		b, err := sess.Init(c.Request.Context())
		if err != nil {
			if os.IsNotExist(err) {
				c.Status(404)
				return
			}
			hlsFail(c, err)
			return
		}
		c.Data(200, "video/mp4", b)
	case strings.HasPrefix(f, "seg") && strings.HasSuffix(f, ".m4s"):
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(f, "seg"), ".m4s"))
		if err != nil {
			c.Status(404)
			return
		}
		b, err := sess.Segment(c.Request.Context(), n)
		if err != nil {
			if os.IsNotExist(err) {
				c.Status(404)
				return
			}
			hlsFail(c, err)
			return
		}
		c.Data(200, "video/mp4", b)
	default:
		c.Status(404)
	}
}

// hlsFail reports a failed init/segment request. A resumed session that lost its slot to
// another player while paused gets 503 (hls.js retries those), not a generic 500.
func hlsFail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, media.ErrBusy):
		c.JSON(503, gin.H{"error": err.Error(), "code": "busy"})
	case errors.Is(err, media.ErrPastEnd):
		c.JSON(410, gin.H{"error": err.Error(), "code": "past_end"})
	default:
		c.JSON(500, gin.H{"error": err.Error()})
	}
}

// hlsPing is the player's heartbeat: it keeps a paused session alive (ffmpeg and its slot are
// released while paused and taken again on resume). A 404 tells the player to open a new one.
func (s *Server) hlsPing(c *gin.Context) {
	sess, ok := s.HLS.Get(c.Param("id"))
	if !ok || !sess.Heartbeat() {
		c.JSON(404, gin.H{"error": "변환 세션이 끝났습니다", "code": "session_gone"})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) hlsClose(c *gin.Context) {
	if sess, ok := s.HLS.Get(c.Param("id")); ok {
		sess.Close()
	}
	c.JSON(200, gin.H{"ok": true})
}
