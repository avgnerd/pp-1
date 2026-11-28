package media

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// shimLauncherSource is a minimal ShimGen-style launcher: it starts the real ffmpeg as a
// child, appends the child PID to SHIM_PID_FILE, inherits stdin/stdout/stderr, and exits with
// the child's status. It deliberately does not forward termination signals, so killing the
// launcher process leaves the real ffmpeg running.
const shimLauncherSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	cmd := exec.Command(os.Getenv("SHIM_FFMPEG"), os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "shim start:", err)
		os.Exit(1)
	}
	if pidFile := os.Getenv("SHIM_PID_FILE"); pidFile != "" {
		f, err := os.OpenFile(pidFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", cmd.Process.Pid)
			_ = f.Close()
		}
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "shim wait:", err)
		os.Exit(1)
	}
}
`

// buildShimLauncher compiles the launcher into a temp dir and returns its path. The manager
// probes it with "-h full" at startup, so the probe is a launcher launch too.
func buildShimLauncher(t *testing.T) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(shimLauncherSource), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "shim-launcher"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command(goTool, "build", "-o", out, "main.go")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build shim launcher: %v\n%s", err, b)
	}
	return out
}

// useShim points later ffmpeg launches at a fresh launcher. It returns the launcher path and
// the file the launcher appends child PIDs to.
func useShim(t *testing.T, ffmpeg string) (shim, pidFile string) {
	t.Helper()
	shim = buildShimLauncher(t)
	pidFile = filepath.Join(t.TempDir(), "shim-pids")
	t.Setenv("SHIM_FFMPEG", ffmpeg)
	t.Setenv("SHIM_PID_FILE", pidFile)
	return shim, pidFile
}

func readPIDs(path string) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(field); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// waitRunPID waits for the real-run line. The first line is the manager's "-h full" probe.
func waitRunPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if pids := readPIDs(path); len(pids) >= 2 {
			return pids[len(pids)-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the ffmpeg child PID in %s (got %v)", path, readPIDs(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killShimPIDs kills every surviving launcher child and waits briefly for it to disappear so a
// killed ffmpeg cannot keep writing into a directory the test is about to remove.
func killShimPIDs(path string) {
	for _, pid := range readPIDs(path) {
		if !processAlive(pid) {
			continue
		}
		p, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		_ = p.Kill()
		deadline := time.Now().Add(3 * time.Second)
		for processAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func waitProcessAlive(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("ffmpeg child pid %d never became alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// longSample makes a seconds-long H.264/AAC file and serves it over HTTP with Range support.
// The resolution is small so a two-minute file still encodes quickly; only the source duration
// matters for keeping the readrate-limited run alive through the pause.
func longSample(t *testing.T, seconds int) string {
	t.Helper()
	ff := tool(t, "ffmpeg")
	p := filepath.Join(t.TempDir(), "long.mkv")
	out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x180:rate=10:duration=%d", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-shortest", p).CombinedOutput()
	if err != nil {
		t.Fatalf("make long sample: %v %s", err, out)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, p)
	}))
	t.Cleanup(ts.Close)
	return ts.URL + "/long.mkv"
}

type hlsFileState struct {
	size int64
	mod  time.Time
}

// snapshotHLSDir records the init, playlist and segment files the player could observe.
func snapshotHLSDir(t *testing.T, dir string) map[string]hlsFileState {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read session dir %s: %v", dir, err)
	}
	snap := make(map[string]hlsFileState, len(entries))
	for _, e := range entries {
		name := e.Name()
		if name != "init.mp4" && name != "live.m3u8" &&
			!(strings.HasPrefix(name, "seg") && strings.HasSuffix(name, ".m4s")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat session file %s: %v", name, err)
		}
		snap[name] = hlsFileState{size: info.Size(), mod: info.ModTime()}
	}
	return snap
}

// changedFiles reports files that appeared or were rewritten between the two snapshots.
func changedFiles(before, after map[string]hlsFileState) []string {
	names := make([]string, 0, len(after))
	for name := range after {
		names = append(names, name)
	}
	sort.Strings(names)
	var changes []string
	for _, name := range names {
		old, ok := before[name]
		now := after[name]
		if !ok {
			changes = append(changes, fmt.Sprintf("%s appeared (size %d)", name, now.size))
			continue
		}
		if old.size != now.size || !old.mod.Equal(now.mod) {
			changes = append(changes, fmt.Sprintf("%s changed (size %d -> %d, mod %s -> %s)",
				name, old.size, now.size, old.mod.Format(time.RFC3339Nano), now.mod.Format(time.RFC3339Nano)))
		}
	}
	return changes
}

// A paused session must not give its transcode slot back while the ffmpeg process tree is still
// running: on Windows the manager kills only its direct child (a ShimGen-style launcher), and
// the launcher does not forward the kill to the real ffmpeg. Releasing the slot then lets another
// player start a transcode while the old encoder still writes to the session directory.
func TestShimProcessTreeStopsBeforeSlotRelease(t *testing.T) {
	ffmpeg := tool(t, "ffmpeg")
	shim, pidFile := useShim(t, ffmpeg)
	m := newManager(shim, filepath.Join(t.TempDir(), "hls"), 1, 300*time.Millisecond)
	t.Cleanup(m.Close)
	t.Cleanup(func() { killShimPIDs(pidFile) })
	if !m.readRate {
		t.Skip("ffmpeg build lacks -readrate_initial_burst; the first run could finish before the pause")
	}

	url := longSample(t, 120)
	s, err := m.Open(Source{Key: "slot", Client: "tab1",
		URL: func(context.Context) (string, error) { return url, nil }, Duration: 120,
		Mode: ModeTranscode, Height: 360, SrcH: 180})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := s.Segment(ctx, 0); err != nil {
		t.Fatalf("first segment: %v", err)
	}
	runPID := waitRunPID(t, pidFile)
	waitProcessAlive(t, runPID)

	// The player is paused but still open: heartbeats keep the session itself alive, so the
	// reaper takes the pause path (stop ffmpeg, free the slot) and not the expire path.
	beat := make(chan struct{})
	defer close(beat)
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-beat:
				return
			case <-tick.C:
				s.Heartbeat()
			}
		}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		active, waiting, limit := m.Limit.Stats()
		if active == 0 {
			alive := processAlive(runPID)
			before := snapshotHLSDir(t, s.dir)
			if alive {
				t.Errorf("transcode slot released while ffmpeg child pid %d was still alive (stats active=%d waiting=%d limit=%d)",
					runPID, active, waiting, limit)
			}
			time.Sleep(1500 * time.Millisecond)
			after := snapshotHLSDir(t, s.dir)
			for _, change := range changedFiles(before, after) {
				t.Errorf("1500ms after the slot was released, encoder output changed: %s", change)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("paused session kept its slot within 15s (stats active=%d waiting=%d limit=%d)",
				active, waiting, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
