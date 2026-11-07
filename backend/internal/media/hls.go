package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/limiter"
	"github.com/dvfs/dropbox-vfs-manager/internal/secret"
)

// Modes.
const (
	ModeRemux     = "remux"     // copy video, AAC audio
	ModeTranscode = "transcode" // H.264 (max height), AAC audio
	ModeAudio     = "audio"     // audio-only AAC
)

// SegmentSeconds is the nominal HLS segment length.
const SegmentSeconds = 4

// ErrBusy is returned when every transcode slot is in use.
var ErrBusy = errors.New("서버의 동시 변환 수가 가득 찼습니다. 다른 재생을 끝낸 뒤 다시 시도하거나 원본으로 재생하세요")

// Source describes what to play.
type Source struct {
	Key      string // stable identity (storage id + file id + rev)
	Client   string // player instance (browser tab); players never share a session
	URL      func(ctx context.Context) (string, error)
	Duration float64
	Mode     string
	Height   int // max output height for transcode (e.g. 1080)
	SrcH     int
	Preset   string
}

// Session is one running HLS output.
type Session struct {
	ID       string
	src      Source
	dir      string
	segments int
	mgr      *Manager

	mu         sync.Mutex
	startMu    sync.Mutex
	closeOnce  sync.Once
	lifeCtx    context.Context
	lifeCancel context.CancelFunc
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	runStart   int       // first segment of the current ffmpeg run
	runAt      time.Time // when the current run started; older segment files are leftovers
	runOK      bool      // the current run exited successfully; its init file is complete
	slot       bool      // holds a transcode slot (while ffmpeg runs, or right after Open)
	endSeg     int       // first segment past the real end of the media (0 = not known)
	exited     chan struct{}
	restarting bool      // a new ffmpeg run is being started
	active     int       // requests currently waiting on this session
	lastAccess time.Time // last playlist/init/segment request
	lastBeat   time.Time // last heartbeat from an open player
	lastSeg    int
	pins       map[int]int // selected segment refs; guarded by mu
	closed     bool
	stderrTail string
}

// Manager owns sessions and bounds concurrency.
type Manager struct {
	FFmpeg  string
	FFprobe string // used to find the keyframe a remux run starts from (optional)
	TempDir string
	Limit   *limiter.Limiter
	// Idle is how long ffmpeg keeps running without segment requests (a paused player); it is
	// then stopped and its transcode slot freed. The session itself lives until there were no
	// requests for Idle and no heartbeat for 3*Idle (background tabs throttle timers to about
	// one per minute). The reaper takes this value when the manager is created; changing the
	// field afterwards does not affect it.
	Idle time.Duration

	readRate bool // ffmpeg supports -readrate_initial_burst

	mu       sync.Mutex
	sessions map[string]*Session
	byKey    map[string]*Session
	stop     chan struct{}
	done     chan struct{}
	closed   bool
	closeOne sync.Once
}

// NewManager creates a manager and starts its idle reaper. The temp dir is wiped first.
func NewManager(ffmpeg, tempDir string, concurrency int) *Manager {
	return newManager(ffmpeg, tempDir, concurrency, 60*time.Second)
}

// newManager creates a manager with the reaper's idle timeout fixed before the goroutine
// starts, so the reaper never reads a field another goroutine can change.
func newManager(ffmpeg, tempDir string, concurrency int, idle time.Duration) *Manager {
	_ = os.RemoveAll(tempDir)
	_ = os.MkdirAll(tempDir, 0o700)
	m := &Manager{FFmpeg: ffmpeg, TempDir: tempDir, Limit: limiter.New(concurrency), Idle: idle,
		sessions: map[string]*Session{}, byKey: map[string]*Session{}, stop: make(chan struct{}), done: make(chan struct{})}
	if ffmpeg != "" {
		cmd := exec.Command(ffmpeg, "-hide_banner", "-h", "full")
		hideWindow(cmd)
		if out, err := cmd.Output(); err == nil && strings.Contains(string(out), "readrate_initial_burst") {
			m.readRate = true
		}
	}
	go m.reaper(idle)
	return m
}

// Available reports whether ffmpeg is configured.
func (m *Manager) Available() bool { return m.FFmpeg != "" }

// Open returns an existing session for the same source+mode or starts a new one.
func (m *Manager) Open(src Source) (*Session, error) {
	if !m.Available() {
		return nil, errors.New("서버에 ffmpeg가 없어 변환 재생을 할 수 없습니다")
	}
	if src.Duration <= 0 {
		return nil, errors.New("영상 길이를 알 수 없어 변환 재생을 할 수 없습니다")
	}
	// The same player reopening the same file (reload, remount) gets its session back; another
	// tab or device gets its own, so seeking or closing in one never disturbs the other.
	key := src.Key + "|" + src.Mode + "|" + strconv.Itoa(src.Height) + "|" + src.Client
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("변환 매니저가 종료되었습니다")
	}
	if s, ok := m.byKey[key]; ok {
		s.mu.Lock()
		closed := s.closed
		if !closed {
			s.lastAccess = time.Now()
			s.lastBeat = time.Now()
		}
		s.mu.Unlock()
		if !closed {
			m.mu.Unlock()
			return s, nil
		}
	}
	if !m.Limit.TryAcquire() {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	id, err := secret.RandomToken(12)
	if err != nil {
		m.mu.Unlock()
		m.Limit.Release()
		return nil, err
	}
	dir := filepath.Join(m.TempDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.mu.Unlock()
		m.Limit.Release()
		return nil, err
	}
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	s := &Session{ID: id, src: src, dir: dir, mgr: m, lastAccess: time.Now(), lastBeat: time.Now(), lastSeg: -1, slot: true,
		segments: segmentCount(src.Duration), pins: map[int]int{}, lifeCtx: lifeCtx, lifeCancel: lifeCancel}
	m.sessions[id] = s
	m.byKey[key] = s
	m.mu.Unlock()
	return s, nil
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Count returns the number of live sessions.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Close stops every session and the reaper.
func (m *Manager) Close() {
	m.closeOne.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.stop)
		<-m.done
		m.mu.Lock()
		ss := make([]*Session, 0, len(m.sessions))
		for _, s := range m.sessions {
			ss = append(ss, s)
		}
		m.mu.Unlock()
		for _, s := range ss {
			s.Close()
		}
	})
}

// StopSessions stops every running transcode session but keeps the manager and its reaper
// alive, so a later request can start a new session. Segment files on disk are left alone: a
// backup restore must not delete files under the thumbs or hls cache directories.
func (m *Manager) StopSessions() {
	m.mu.Lock()
	ss := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		ss = append(ss, s)
	}
	m.sessions = map[string]*Session{}
	m.byKey = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range ss {
		s.shutdown(false)
	}
}

func (m *Manager) reaper(idle time.Duration) {
	defer close(m.done)
	tick := 10 * time.Second
	if idle/2 < tick {
		tick = max(idle/2, 50*time.Millisecond)
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.mu.Lock()
			var gone, pausedRuns, running []*Session
			for _, s := range m.sessions {
				s.mu.Lock()
				sinceUse := time.Since(s.lastAccess)
				sinceBeat := time.Since(s.lastBeat)
				switch {
				case s.active > 0:
					// A request is waiting for ffmpeg; never reap under it.
				case sinceUse > idle && sinceBeat > 3*idle:
					// No requests and no open player: the tab is gone.
					gone = append(gone, s)
				case sinceUse > idle && (s.cmd != nil || s.slot):
					// The player is open but paused: stop encoding and free the slot, keep the session.
					pausedRuns = append(pausedRuns, s)
				case s.cmd != nil:
					running = append(running, s)
				}
				s.mu.Unlock()
			}
			m.mu.Unlock()
			for _, s := range gone {
				s.Close()
			}
			// Checks are repeated under startMu: a request may have arrived since the scan, and
			// stopping the run under it would fail that request.
			for _, s := range pausedRuns {
				s.startMu.Lock()
				s.mu.Lock()
				paused := s.active == 0 && time.Since(s.lastAccess) > idle
				s.mu.Unlock()
				if paused {
					s.stopRun(false)
				}
				s.startMu.Unlock()
			}
			// A run far ahead of the playhead is stopped; the next missing segment restarts it.
			for _, s := range running {
				s.startMu.Lock()
				s.mu.Lock()
				last := max(s.lastSeg, s.runStart)
				since := s.runAt
				quiet := s.active == 0 && s.cmd != nil
				s.mu.Unlock()
				if quiet && s.highestSegment(since) > last+aheadLimit {
					// Keep the slot: the viewer is still watching and will need it again soon.
					s.stopRun(true)
				}
				s.startMu.Unlock()
			}
		}
	}
}

func (s *Session) begin() {
	s.mu.Lock()
	s.active++
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

func (s *Session) end() {
	s.mu.Lock()
	s.active--
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

// Heartbeat keeps the session alive while a player has it open (also while paused).
// It reports false when the session has already been closed.
func (s *Session) Heartbeat() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.lastBeat = time.Now()
	return true
}

// Close kills ffmpeg, removes files and frees the slot.
func (s *Session) Close() {
	s.shutdown(true)
}

func (s *Session) shutdown(removeDir bool) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		cancel := s.lifeCancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}

		s.startMu.Lock()
		s.stopRun(false)
		if removeDir {
			_ = os.RemoveAll(s.dir)
		}
		s.startMu.Unlock()
		m := s.mgr
		m.mu.Lock()
		delete(m.sessions, s.ID)
		for k, v := range m.byKey {
			if v == s {
				delete(m.byKey, k)
			}
		}
		m.mu.Unlock()
	})
}

func (s *Session) canceled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return true
	}
	if s.lifeCtx == nil {
		return false
	}
	select {
	case <-s.lifeCtx.Done():
		return true
	default:
		return false
	}
}

// Segments returns the total segment count.
func (s *Session) Segments() int { return s.segments }

// tailSlack is how far past a segment boundary the media must reach for ffmpeg to be sure to
// write that segment: the last packet starts up to one frame (or audio packet) before the end.
const tailSlack = 0.1

// segmentCount is the number of segments ffmpeg writes for media ending at end seconds.
func segmentCount(end float64) int {
	return max(1, int(math.Ceil((end-tailSlack)/SegmentSeconds)))
}

// ErrPastEnd is returned for playlist segments after the real end of the media (the probed
// duration was longer than what ffmpeg could cut, e.g. audio outlasting the video).
var ErrPastEnd = errors.New("영상이 이 구간 전에 끝납니다")

// Playlist returns the full VOD playlist with fixed-length segments.
func (s *Session) Playlist() string {
	s.touch()
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", SegmentSeconds+2)
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:0\n")
	if s.src.Mode != ModeRemux {
		// Copied video is cut by time, so its segments may not start on a keyframe.
		b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	}
	b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
	remaining := s.src.Duration
	for i := 0; i < s.segments; i++ {
		d := math.Min(SegmentSeconds, remaining)
		if i == s.segments-1 {
			d = math.Max(remaining, 0.001) // a tail under tailSlack rides in the last segment
		}
		remaining -= d
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg%d.m4s\n", d, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func (s *Session) segPath(n int) string { return filepath.Join(s.dir, fmt.Sprintf("seg%d.m4s", n)) }
func (s *Session) initPath() string     { return filepath.Join(s.dir, "init.mp4") }

// pinSegment retains segment n until releaseSegment. The map is guarded by mu; cleanup takes
// a snapshot under mu before removing files.
func (s *Session) pinSegment(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pins == nil {
		s.pins = map[int]int{}
	}
	s.pins[n]++
}

func (s *Session) releaseSegment(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pins[n] <= 1 {
		delete(s.pins, n)
		return
	}
	s.pins[n]--
}

// lookahead is how far ahead of a request an existing run may be before we just wait for it.
const lookahead = 6

// aheadLimit is how many segments (4 s each, so 5 minutes) a run may get ahead of the playhead
// before it is stopped. cleanupOld keeps up to 90 ahead, so stopped runs lose nothing.
const aheadLimit = 75

// Init waits for and returns the init segment bytes. It also waits for the first media
// segment of the current run so the init file is known to be completely written.
func (s *Session) Init(ctx context.Context) ([]byte, error) {
	s.begin()
	defer s.end()
	pinned := -1
	releasePinned := func() {
		if pinned >= 0 {
			s.releaseSegment(pinned)
			pinned = -1
		}
	}
	defer func() {
		releasePinned()
	}()
	for {
		s.startMu.Lock()
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			s.startMu.Unlock()
			return nil, errors.New("세션이 종료되었습니다")
		}
		running := s.cmd != nil
		runStart := s.runStart
		runAt := s.runAt
		exited := s.exited
		runOK := s.runOK
		s.mu.Unlock()

		if !running && fileExistsAfter(s.initPath(), runAt) &&
			(runOK || fileExistsAfter(s.segPath(runStart), runAt)) {
			b, err := os.ReadFile(s.initPath())
			s.startMu.Unlock()
			return b, err
		}
		releasePinned()
		if !running {
			s.pinSegment(0)
			pinned = 0
			if err := s.start(0); err != nil {
				s.startMu.Unlock()
				return nil, err
			}
			s.mu.Lock()
			runStart = s.runStart
			runAt = s.runAt
			exited = s.exited
			s.mu.Unlock()
		} else {
			s.pinSegment(runStart)
			pinned = runStart
		}
		s.startMu.Unlock()

		err := s.waitForInitRun(ctx, runStart, runAt, exited)
		if err == errInitRunReplaced {
			releasePinned()
			continue
		}
		if err != nil {
			return nil, err
		}

		s.startMu.Lock()
		s.mu.Lock()
		closed := s.closed
		sameRun := s.exited == exited && !s.restarting && s.runStart == runStart && s.runAt.Equal(runAt)
		s.mu.Unlock()
		ready := !closed && sameRun && fileExistsAfter(s.segPath(runStart), runAt) &&
			fileExistsAfter(s.initPath(), runAt)
		if ready {
			b, err := os.ReadFile(s.initPath())
			s.startMu.Unlock()
			return b, err
		}
		releasePinned()
		s.startMu.Unlock()
		if closed {
			return nil, errors.New("세션이 종료되었습니다")
		}
	}
}

var errInitRunReplaced = errors.New("init run replaced")

func (s *Session) waitForInitRun(ctx context.Context, runStart int, runAt time.Time, exited <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		s.mu.Lock()
		current := s.exited
		restarting := s.restarting
		tail := s.stderrTail
		var lifeDone <-chan struct{}
		if s.lifeCtx != nil {
			lifeDone = s.lifeCtx.Done()
		}
		s.mu.Unlock()
		if current != exited || restarting {
			return errInitRunReplaced
		}
		if fileExistsAfter(s.segPath(runStart), runAt) && fileExistsAfter(s.initPath(), runAt) {
			return nil
		}
		if exited != nil {
			select {
			case <-exited:
				s.mu.Lock()
				current = s.exited
				restarting = s.restarting
				tail = s.stderrTail
				s.mu.Unlock()
				if current != exited || restarting {
					return errInitRunReplaced
				}
				if fileExistsAfter(s.segPath(runStart), runAt) && fileExistsAfter(s.initPath(), runAt) {
					return nil
				}
				if tail != "" {
					return fmt.Errorf("ffmpeg가 종료되었습니다: %s", tail)
				}
				return errors.New("ffmpeg가 이 구간을 만들지 못하고 종료되었습니다")
			default:
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("변환 대기 시간 초과: %w", ctx.Err())
		case <-lifeDone:
			return errors.New("세션이 종료되었습니다")
		case <-t.C:
		}
	}
}

// Segment returns the bytes of segment n, starting or restarting ffmpeg as needed.
func (s *Session) Segment(ctx context.Context, n int) ([]byte, error) {
	if n < 0 || n >= s.segments {
		return nil, os.ErrNotExist
	}
	// A request that waits for ffmpeg counts as activity until it finishes.
	s.begin()
	defer s.end()
	s.startMu.Lock()
	s.mu.Lock()
	s.lastAccess = time.Now()
	s.lastSeg = n
	if s.closed {
		s.mu.Unlock()
		s.startMu.Unlock()
		return nil, errors.New("세션이 종료되었습니다")
	}
	running := s.cmd != nil
	runStart := s.runStart
	pastEnd := s.endSeg > 0 && n >= s.endSeg
	s.mu.Unlock()
	if pastEnd {
		s.startMu.Unlock()
		return nil, ErrPastEnd
	}
	s.pinSegment(n)
	defer s.releaseSegment(n)
	p := s.segPath(n)
	b, complete, err := readCompletedSegment(p)
	if err != nil {
		s.startMu.Unlock()
		return nil, err
	}
	if complete {
		s.cleanupOldLocked(n)
		s.startMu.Unlock()
		return b, nil
	}
	s.mu.Lock()
	runAt := s.runAt
	s.mu.Unlock()
	produced := s.highestSegment(time.Time{})
	if running {
		// Only segments written by the current run count: files left from an earlier run
		// (before a seek back) say nothing about how far this one got.
		produced = s.highestSegment(runAt)
	}
	// Restart when nothing is running, when the request is behind the current run (seek back),
	// far ahead of it (seek forward), or already passed by it: segments ahead of the playhead
	// are cleaned up, so a missing segment below the high-water mark will never appear.
	passed := running && n >= runStart && n <= produced
	if !fileExists(p) && (!running || n < runStart || n > max(produced, runStart)+lookahead || passed) {
		if err := s.start(n); err != nil {
			s.startMu.Unlock()
			return nil, err
		}
	}
	s.startMu.Unlock()
	if err := s.waitFor(ctx, p); err != nil {
		s.startMu.Lock()
		s.mu.Lock()
		pastEnd := s.endSeg > 0 && n >= s.endSeg
		s.mu.Unlock()
		s.startMu.Unlock()
		if pastEnd {
			return nil, ErrPastEnd
		}
		return nil, err
	}
	s.startMu.Lock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.startMu.Unlock()
		return nil, errors.New("세션이 종료되었습니다")
	}
	pastEnd = s.endSeg > 0 && n >= s.endSeg
	s.mu.Unlock()
	if pastEnd {
		s.startMu.Unlock()
		return nil, ErrPastEnd
	}
	b, complete, err = readCompletedSegment(p)
	s.cleanupOldLocked(n)
	s.startMu.Unlock()
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, os.ErrNotExist
	}
	return b, nil
}

// highestSegment returns the highest segment number on disk written at or after since.
func (s *Session) highestSegment(since time.Time) int {
	hi := -1
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "seg") && strings.HasSuffix(name, ".m4s") {
			if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg"), ".m4s")); err == nil && n > hi {
				if !since.IsZero() {
					info, err := e.Info()
					if err != nil || info.ModTime().Before(since) {
						continue
					}
				}
				hi = n
			}
		}
	}
	return hi
}

// cleanupOld removes segments well behind the playhead so disk use stays bounded.
func (s *Session) cleanupOld(n int) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.cleanupOldLocked(n)
}

// cleanupOldLocked removes segments well behind the playhead while startMu prevents new
// selections and directory removal. Callers must hold startMu.
func (s *Session) cleanupOldLocked(n int) {
	s.mu.Lock()
	keep := -1
	if s.cmd != nil {
		keep = s.runStart
	}
	pinned := make(map[int]struct{}, len(s.pins))
	for k := range s.pins {
		pinned[k] = struct{}{}
	}
	s.mu.Unlock()

	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "seg") || !strings.HasSuffix(name, ".m4s") {
			continue
		}
		k, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg"), ".m4s"))
		_, pin := pinned[k]
		if err == nil && k != keep && !pin && (k < n-15 || k > n+90) {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
}

func (s *Session) waitFor(ctx context.Context, p string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		if fileExists(p) {
			return nil
		}
		s.mu.Lock()
		exited := s.exited
		tail := s.stderrTail
		var lifeDone <-chan struct{}
		if s.lifeCtx != nil {
			lifeDone = s.lifeCtx.Done()
		}
		s.mu.Unlock()
		if exited != nil {
			select {
			case <-exited:
				if fileExists(p) {
					return nil
				}
				// A seek restart or a pause stop replaced the run we were watching; keep waiting
				// for the current one (the request context ends it if the player gave up).
				s.mu.Lock()
				replaced := s.exited != exited || s.restarting
				s.mu.Unlock()
				if replaced {
					break
				}
				if tail != "" {
					return fmt.Errorf("ffmpeg가 종료되었습니다: %s", tail)
				}
				return errors.New("ffmpeg가 이 구간을 만들지 못하고 종료되었습니다")
			default:
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("변환 대기 시간 초과: %w", ctx.Err())
		case <-lifeDone:
			return errors.New("세션이 종료되었습니다")
		case <-t.C:
		}
	}
}

func (s *Session) stopRun(keepSlot bool) {
	s.mu.Lock()
	cancel := s.cancel
	exited := s.exited
	s.cmd = nil
	s.cancel = nil
	release := s.slot && !keepSlot
	if release {
		s.slot = false
	}
	s.mu.Unlock()
	if release {
		s.mgr.Limit.Release()
	}
	if cancel != nil {
		cancel()
		if exited != nil {
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// Args builds the ffmpeg command line for a run requested at segment n. For copied video the
// run really starts at a keyframe at or before n*SegmentSeconds; startNum and offset shift that
// run so its first packet lands exactly on a segment boundary, which keeps every segment k at
// [k*4, k*4+4) as the pre-built playlist promises (offset is under 4 s).
func (s *Session) Args(inputURL string, n, startNum int, offset float64) []string {
	start := float64(n * SegmentSeconds)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-reconnect", "1", "-reconnect_on_network_error", "1", "-reconnect_delay_max", "5"}
	if s.mgr.readRate {
		// Stay at most 2x ahead of real time after a 60 s burst: no point downloading or
		// encoding a whole movie the viewer may never reach.
		args = append(args, "-readrate", "2", "-readrate_initial_burst", "60")
	}
	if s.src.Mode == ModeRemux {
		// Copied video starts at a keyframe; audio must start there too. With accurate seeking
		// the re-encoded audio starts later and its timestamps can restart near zero.
		args = append(args, "-noaccurate_seek")
	}
	if start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
	}
	args = append(args, "-i", inputURL, "-copyts", "-avoid_negative_ts", "disabled", "-start_at_zero")
	flags := "temp_file+independent_segments"
	switch s.src.Mode {
	case ModeAudio:
		args = append(args, "-map", "0:a:0", "-vn", "-c:a", "aac", "-b:a", "256k", "-ac", "2")
	case ModeRemux:
		args = append(args, "-map", "0:V:0", "-map", "0:a:0?", "-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-ac", "2")
		// Copied video keeps its own keyframes; cut by time instead of waiting for them.
		flags = "temp_file+split_by_time"
		if offset > 0 {
			args = append(args, "-output_ts_offset", strconv.FormatFloat(offset, 'f', 3, 64))
		}
	default:
		preset := s.src.Preset
		if preset == "" {
			preset = "veryfast"
		}
		h := s.src.Height
		if h <= 0 {
			h = 1080
		}
		vf := "format=yuv420p"
		if s.src.SrcH == 0 || s.src.SrcH > h {
			vf = fmt.Sprintf("scale=-2:'min(%d,ih)',format=yuv420p", h)
		}
		args = append(args, "-map", "0:V:0", "-map", "0:a:0?",
			"-c:v", "libx264", "-preset", preset, "-crf", "21", "-maxrate", "10M", "-bufsize", "20M",
			"-profile:v", "high", "-level:v", "4.1", "-vf", vf,
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", SegmentSeconds),
			"-sc_threshold", "0", "-threads", "0",
			"-c:a", "aac", "-b:a", "192k", "-ac", "2")
	}
	args = append(args, "-max_muxing_queue_size", "2048",
		"-f", "hls", "-hls_time", strconv.Itoa(SegmentSeconds), "-hls_list_size", "0",
		"-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4",
		"-hls_flags", flags,
		"-start_number", strconv.Itoa(startNum),
		// Paths are relative: ffmpeg runs with the session directory as its working dir,
		// which is the only way the fMP4 init file lands next to the segments.
		"-hls_segment_filename", "seg%d.m4s",
		"live.m3u8")
	return args
}

func (s *Session) start(n int) error {
	s.mu.Lock()
	s.restarting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}()
	s.stopRun(true)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("세션이 종료되었습니다")
	}
	lifeCtx := s.lifeCtx
	if lifeCtx == nil {
		s.mu.Unlock()
		return errors.New("세션 수명 주기를 시작할 수 없습니다")
	}
	needSlot := !s.slot
	s.mu.Unlock()
	ok := false
	defer func() {
		if !ok {
			s.stopRun(false)
		}
	}()
	// A paused session gave its slot back; take one again before encoding.
	if needSlot {
		if !s.mgr.Limit.TryAcquire() {
			return ErrBusy
		}
		s.mu.Lock()
		s.slot = true
		s.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(lifeCtx)
	url, err := s.src.URL(ctx)
	if err != nil {
		cancel()
		return err
	}
	if s.canceled() || ctx.Err() != nil {
		cancel()
		return errors.New("세션이 종료되었습니다")
	}
	startNum, offset := n, 0.0
	if s.src.Mode == ModeRemux && n > 0 {
		at := float64(n * SegmentSeconds)
		k, err := s.mgr.runStartTime(ctx, url, at)
		if err != nil {
			cancel()
			return fmt.Errorf("remux 구간의 키프레임을 찾지 못했습니다: %w", err)
		}
		if k < 0 || k > at+0.001 {
			cancel()
			return fmt.Errorf("remux 구간의 키프레임 시각이 잘못되었습니다: %.3f", k)
		}
		startNum = int(math.Ceil(k/SegmentSeconds - 1e-6))
		if startNum > n {
			startNum = n
		}
		if offset = float64(startNum*SegmentSeconds) - k; offset < 0.001 {
			offset = 0
		}
	}
	if s.canceled() || ctx.Err() != nil {
		cancel()
		return errors.New("세션이 종료되었습니다")
	}
	cmd := exec.CommandContext(ctx, s.mgr.FFmpeg, s.Args(url, n, startNum, offset)...)
	cmd.Dir = s.dir
	hideWindow(cmd)
	cmd.WaitDelay = 3 * time.Second
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	runAt := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("ffmpeg 실행 실패: %w", err)
	}
	exited := make(chan struct{})
	s.mu.Lock()
	s.cmd = cmd
	s.cancel = cancel
	s.runStart = startNum
	s.runAt = runAt
	s.runOK = false
	s.exited = exited
	s.stderrTail = ""
	s.mu.Unlock()
	ok = true
	go func() {
		sc := bufio.NewScanner(io.LimitReader(stderr, 1<<20))
		var tail []string
		for sc.Scan() {
			tail = append(tail, sc.Text())
			if len(tail) > 5 {
				tail = tail[1:]
			}
		}
		err := cmd.Wait()
		// A run that read the whole input ends the media: segments it did not write after its
		// last one never exist (only trusted near the end, a cut connection can also exit 0).
		end := 0
		if err == nil && ctx.Err() == nil {
			if e := max(s.highestSegment(runAt)+1, startNum); e < s.segments && e >= s.segments-3 {
				end = e
			}
		}
		s.mu.Lock()
		s.stderrTail = strings.Join(tail, " | ")
		if s.exited == exited && err == nil && ctx.Err() == nil {
			s.runOK = true
		}
		if end > 0 && (s.endSeg == 0 || end < s.endSeg) {
			s.endSeg = end
		}
		release := false
		if s.cmd == cmd {
			// Finished (or crashed) on its own: nothing is encoding, so free the slot.
			s.cmd = nil
			s.cancel = nil
			release = s.slot
			s.slot = false
		}
		s.mu.Unlock()
		if release {
			s.mgr.Limit.Release()
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("ffmpeg exited", "session", s.ID, "err", err, "stderr", strings.Join(tail, " | "))
		}
		cancel()
		close(exited)
	}()
	if s.canceled() || ctx.Err() != nil {
		s.stopRun(false)
		return errors.New("세션이 종료되었습니다")
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

func fileExistsAfter(p string, since time.Time) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0 && (since.IsZero() || !st.ModTime().Before(since))
}

func readCompletedSegment(p string) ([]byte, bool, error) {
	if !fileExists(p) {
		return nil, false, nil
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(b) == 0 {
		return nil, false, nil
	}
	return b, true, nil
}

const runStartTimeAttempts = 3

// runStartTime reports where a copy-mode run seeking to at really starts: the timestamp of the
// first video packet ffmpeg emits, i.e. the keyframe at or before at, on the same timeline as
// the HLS output (-copyts -start_at_zero). A transient source failure is retried; after the
// bounded attempts the caller must fail rather than silently start from a different keyframe.
func (m *Manager) runStartTime(ctx context.Context, url string, at float64) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < runStartTimeAttempts; attempt++ {
		k, err := m.runStartTimeOnce(ctx, url, at)
		if err == nil {
			return k, nil
		}
		lastErr = err
		if attempt+1 == runStartTimeAttempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
	return 0, fmt.Errorf("키프레임 조회가 %d회 실패했습니다: %w", runStartTimeAttempts, lastErr)
}

func (m *Manager) runStartTimeOnce(ctx context.Context, url string, at float64) (float64, error) {
	cmd := exec.CommandContext(ctx, m.FFmpeg, "-hide_banner", "-v", "error", "-nostdin",
		"-ss", strconv.FormatFloat(at, 'f', 3, 64), "-i", url,
		"-copyts", "-avoid_negative_ts", "disabled", "-start_at_zero",
		"-map", "0:V:0", "-c", "copy", "-frames:v", "1", "-f", "framecrc", "-")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	// framecrc lines: "#tb 0: 1/1000" then "0, <dts>, <pts>, <duration>, <size>, <crc>".
	tb := 0.0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#tb 0:") {
			var num, den float64
			if _, err := fmt.Sscanf(strings.TrimSpace(line[len("#tb 0:"):]), "%f/%f", &num, &den); err == nil && den > 0 {
				tb = num / den
			}
			continue
		}
		if tb > 0 && strings.HasPrefix(line, "0,") {
			f := strings.Split(line, ",")
			if len(f) >= 3 {
				pts, err := strconv.ParseFloat(strings.TrimSpace(f[2]), 64)
				if err == nil {
					return pts * tb, nil
				}
			}
		}
	}
	return 0, errors.New("첫 프레임 시각을 읽지 못했습니다")
}
