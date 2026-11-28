package media

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The wave2 regressions run the test binary as a fake ffmpeg (os.Args[0] plus an env switch), so
// the helper ignores the ffmpeg argument list. TestMain dispatches on wave2Helper.
const (
	wave2Helper        = "HLS_WAVE2_HELPER"
	wave2HelperMode    = "HLS_WAVE2_HELPER_MODE"
	wave2HelperReady   = "HLS_WAVE2_HELPER_READY"
	wave2HelperRelease = "HLS_WAVE2_HELPER_RELEASE"
)

// hlsWave2Helper is the fake ffmpeg for the wave2 HLS regressions.
func hlsWave2Helper() int {
	for _, arg := range os.Args[1:] {
		// NewManager probes `ffmpeg -h full` to detect -readrate_initial_burst.
		if arg == "-h" {
			return 0
		}
	}
	switch os.Getenv(wave2HelperMode) {
	case "stderr-flood":
		// One line past bufio.Scanner's 64 KiB token limit, then more than the 1 MiB the
		// stderr reader consumes before it stops, so a run that stops reading leaves ffmpeg
		// blocked writing to a full pipe before it produces any output.
		w := bufio.NewWriterSize(os.Stderr, 64<<10)
		if _, err := w.WriteString(strings.Repeat("e", 96<<10) + "\n"); err != nil {
			return 2
		}
		for range 24 {
			if _, err := w.WriteString(strings.Repeat("f", 64<<10) + "\n"); err != nil {
				return 2
			}
		}
		if err := w.Flush(); err != nil {
			return 2
		}
		if !wave2WriteFile("init.mp4", "wave2 init") || !wave2WriteFile("seg0.m4s", "wave2 segment 0") {
			return 2
		}
		return 0
	case "segments":
		const segments = 94
		for i := 0; i < segments; i++ {
			if !wave2WriteFile(fmt.Sprintf("seg%d.m4s", i), fmt.Sprintf("wave2 segment %d", i)) {
				return 2
			}
		}
		ready, release := os.Getenv(wave2HelperReady), os.Getenv(wave2HelperRelease)
		if ready == "" || release == "" || os.WriteFile(ready, []byte("ready"), 0o600) != nil {
			return 2
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(release); err == nil {
				return 0
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 2
	}
	return 2
}

func wave2WriteFile(name, value string) bool {
	return os.WriteFile(name, []byte(value), 0o600) == nil
}

func waitWave2File(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
