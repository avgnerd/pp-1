package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
)

// TestScale150Accounts connects 150 storages and drives browsing, search, folder sizes and
// streaming concurrently, then checks that goroutines return to their starting level.
// Run with: go test ./internal/server -run Scale -v   (skipped with -short)
func TestScale150Accounts(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	const n = 150
	e := newEnv(t)
	e.login()
	e.fake.Latency = 5 * time.Millisecond
	for i := 1; i <= n; i++ {
		key := fmt.Sprintf("acct%03d", i)
		e.connect(key, fmt.Sprintf("/s/%03d", i))
		a := e.fake.Account(key)
		for j := 0; j < 40; j++ {
			a.Put(fmt.Sprintf("/videos/clip %02d.mp4", j), make([]byte, 4096), time.Now())
		}
		a.Put("/docs/report.txt", []byte("hello"), time.Now())
	}
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	baseG := runtime.NumGoroutine()
	var mem0 runtime.MemStats
	runtime.ReadMemStats(&mem0)

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2000)
	get := func(path string) {
		resp, err := e.c.Get(e.ts.URL + path)
		if err != nil {
			errs <- err
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 && resp.StatusCode != 404 {
			errs <- fmt.Errorf("%s -> %d", path, resp.StatusCode)
		}
	}
	// Browsing every storage, twice (second pass should hit the cache).
	for pass := 0; pass < 2; pass++ {
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				get("/api/fs/list?path=" + url.QueryEscape(fmt.Sprintf("/s/%03d/videos", i)) + "&limit=30")
			}(i)
		}
		wg.Wait()
	}
	listCalls := e.fake.CallCount("files/list_folder")
	// Streaming (proxy) and folder sizes in parallel with a fan-out search.
	for i := 1; i <= 40; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			get("/api/fs/raw?proxy=1&path=" + url.QueryEscape(fmt.Sprintf("/s/%03d/videos/clip 01.mp4", i)))
		}(i)
		go func(i int) {
			defer wg.Done()
			get("/api/fs/folder-size?path=" + url.QueryEscape(fmt.Sprintf("/s/%03d/videos", i)))
		}(i)
	}
	var results int
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := e.c.Get(e.ts.URL + "/api/search?kind=file&q=report")
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		ev := ""
		for sc.Scan() {
			l := sc.Text()
			if strings.HasPrefix(l, "event: ") {
				ev = l[7:]
			} else if ev == "results" && strings.HasPrefix(l, "data: ") {
				results += strings.Count(l, `"type":"file"`)
			} else if ev == "done" {
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	elapsed := time.Since(start)
	if listCalls != n {
		t.Errorf("list_folder calls = %d, want %d (second pass should be cached)", listCalls, n)
	}
	if results != n {
		t.Errorf("search results = %d, want %d", results, n)
	}

	// Let idle connections and handlers wind down, then compare goroutines. Pooled keep-alive
	// connections to Dropbox (two goroutines each) are closed first so only leaks remain.
	e.c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil && runtime.NumGoroutine() > baseG+10 {
		dropbox.SharedTransport.CloseIdleConnections()
		time.Sleep(100 * time.Millisecond)
	}
	runtime.GC()
	var mem1 runtime.MemStats
	runtime.ReadMemStats(&mem1)
	g := runtime.NumGoroutine()
	t.Logf("150 accounts: %v elapsed, goroutines %d -> %d, heap %d KB -> %d KB, cache %+v",
		elapsed.Round(time.Millisecond), baseG, g, mem0.HeapAlloc>>10, mem1.HeapAlloc>>10, e.srv.Cache.Stats())
	if g > baseG+10 {
		t.Errorf("goroutines did not settle: %d -> %d", baseG, g)
	}
	_ = http.StatusOK
}
