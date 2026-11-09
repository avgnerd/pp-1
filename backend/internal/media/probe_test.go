package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

const expectedProbeProcessCap = 4

func newProbeTestHarness(t *testing.T) (*Prober, string) {
	t.Helper()
	ffmpeg := tool(t, "ffmpeg")
	ffprobe := tool(t, "ffprobe")
	dir := testutil.TempDir(t)
	mediaPath := filepath.Join(dir, "probe.mkv")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", mediaPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make probe sample: %v %s", err, out)
	}
	d, err := db.OpenPath(filepath.Join(dir, "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return &Prober{DB: d, FFprobe: ffprobe}, mediaPath
}

func blockingProbeServer(t *testing.T, mediaPath string) (string, *atomic.Int32, *atomic.Int32, <-chan struct{}, func()) {
	t.Helper()
	var active, maximum atomic.Int32
	started := make(chan struct{}, 32)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if cur <= old || maximum.CompareAndSwap(old, cur) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			http.ServeFile(w, r, mediaPath)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(releaseAll)
	return srv.URL + "/probe.mkv", &active, &maximum, started, releaseAll
}

func TestProbeCoalescesConcurrentCalls(t *testing.T) {
	p, mediaPath := newProbeTestHarness(t)
	url, _, maximum, started, release := blockingProbeServer(t, mediaPath)
	defer release()

	const callers = 8
	begin := make(chan struct{})
	results := make(chan error, callers)
	for range callers {
		go func() {
			<-begin
			_, err := p.Probe(context.Background(), "same-file", "same-rev", "probe.mkv", url)
			results <- err
		}()
	}
	close(begin)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("ffprobe did not start")
	}
	time.Sleep(200 * time.Millisecond)
	concurrent := maximum.Load()
	t.Logf("concurrent_ffprobe_requests=%d duplicate_key=same-file/same-rev", concurrent)
	release()

	for range callers {
		if err := <-results; err != nil {
			t.Errorf("probe failed: %v", err)
		}
	}
	if concurrent != 1 {
		t.Fatalf("concurrent ffprobe requests = %d, want 1", concurrent)
	}

	pr, err := p.Probe(context.Background(), "same-file", "same-rev", "probe.mkv", "http://127.0.0.1:1/missing")
	if err != nil || pr.Video == nil || pr.Video.CodecName != "h264" {
		t.Fatalf("cached probe after coalesced call: %+v, %v", pr, err)
	}
}

func TestProbeBoundsDistinctConcurrentCalls(t *testing.T) {
	p, mediaPath := newProbeTestHarness(t)
	url, _, maximum, started, release := blockingProbeServer(t, mediaPath)
	defer release()

	const callers = expectedProbeProcessCap + 2
	begin := make(chan struct{})
	results := make(chan error, callers)
	for i := range callers {
		go func(i int) {
			<-begin
			_, err := p.Probe(context.Background(), fmt.Sprintf("file-%d", i), "rev", "probe.mkv", fmt.Sprintf("%s?id=%d", url, i))
			results <- err
		}(i)
	}
	close(begin)

	for range expectedProbeProcessCap {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d ffprobe requests reached cap", maximum.Load())
		}
	}
	time.Sleep(200 * time.Millisecond)
	concurrent := maximum.Load()
	t.Logf("concurrent_ffprobe_requests=%d distinct_keys=%d", concurrent, callers)
	release()

	for range callers {
		if err := <-results; err != nil {
			t.Errorf("probe failed: %v", err)
		}
	}
	if concurrent != expectedProbeProcessCap {
		t.Fatalf("concurrent ffprobe requests = %d, want %d", concurrent, expectedProbeProcessCap)
	}
}

func TestProbeCanceledWaiterDoesNotCancelSharedRun(t *testing.T) {
	p, mediaPath := newProbeTestHarness(t)
	url, _, _, started, release := blockingProbeServer(t, mediaPath)
	defer release()

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	begin := make(chan struct{})
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() {
		<-begin
		_, err := p.Probe(firstCtx, "same-file", "rev", "probe.mkv", url)
		first <- err
	}()
	go func() {
		<-begin
		_, err := p.Probe(secondCtx, "same-file", "rev", "probe.mkv", url)
		second <- err
	}()
	close(begin)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("ffprobe did not start")
	}

	cancelFirst()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled caller did not return promptly")
	}
	release()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("remaining caller failed after peer cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remaining caller did not finish")
	}
}

func TestProbeDoesNotStartCanceledCall(t *testing.T) {
	p, mediaPath := newProbeTestHarness(t)
	url, _, maximum, _, release := blockingProbeServer(t, mediaPath)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Probe(ctx, "file", "rev", "probe.mkv", url); !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe error = %v, want context.Canceled", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := maximum.Load(); got != 0 {
		t.Fatalf("canceled call started %d ffprobe requests", got)
	}
}

func TestProbeRetriesAfterFailure(t *testing.T) {
	p, mediaPath := newProbeTestHarness(t)
	bad := httptest.NewServer(http.NotFoundHandler())
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, mediaPath)
	}))
	defer good.Close()

	if _, err := p.Probe(context.Background(), "retry-file", "rev", "probe.mkv", bad.URL+"/probe.mkv"); err == nil {
		t.Fatal("first probe unexpectedly succeeded")
	}
	pr, err := p.Probe(context.Background(), "retry-file", "rev", "probe.mkv", good.URL+"/probe.mkv")
	if err != nil || pr.Video == nil {
		t.Fatalf("retry did not run ffprobe: %+v, %v", pr, err)
	}
	if pr, err := p.Probe(context.Background(), "retry-file", "rev", "probe.mkv", bad.URL+"/probe.mkv"); err != nil || pr.Video == nil {
		t.Fatalf("successful retry was not cached: %+v, %v", pr, err)
	}
}
