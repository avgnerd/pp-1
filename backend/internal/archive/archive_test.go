package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/archive"
	"github.com/dvfs/dropbox-vfs-manager/internal/stream"
)

func makeZip(t *testing.T, files int, size int) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	data := make([]byte, size)
	rand.Read(data)
	for i := 0; i < files; i++ {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("폴더/sub/file%03d.bin", i), Method: zip.Store, Modified: time.Now()})
		w.Write(data)
	}
	zw.Close()
	return buf.Bytes()
}

func makeTar(t *testing.T, gz bool) []byte {
	var buf bytes.Buffer
	var tw *tar.Writer
	var gw *gzip.Writer
	if gz {
		gw = gzip.NewWriter(&buf)
		tw = tar.NewWriter(gw)
	} else {
		tw = tar.NewWriter(&buf)
	}
	data := make([]byte, 1<<20)
	for i := 0; i < 5; i++ {
		tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("dir/f%d.bin", i), Size: int64(len(data)), Mode: 0o644, ModTime: time.Now()})
		tw.Write(data)
	}
	tw.Close()
	if gw != nil {
		gw.Close()
	}
	return buf.Bytes()
}

func TestZipOverRangeReadsOnlyTail(t *testing.T) {
	data := makeZip(t, 50, 200<<10) // ~10 MB
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.zip", time.Now(), bytes.NewReader(data))
	}))
	defer ts.Close()
	ra := stream.NewReaderAt(context.Background(), http.DefaultClient, int64(len(data)),
		func(context.Context) (string, error) { return ts.URL, nil }, nil)
	l, err := archive.List("zip", ra, int64(len(data)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range l.Entries {
		if !e.Dir {
			files++
		}
	}
	if files != 50 {
		t.Fatalf("files %d", files)
	}
	// Implicit folders are added.
	if l.Entries[0].Name != "폴더/" || !l.Entries[0].Dir {
		t.Fatalf("first entry %+v", l.Entries[0])
	}
	if ra.Reads > 4 {
		t.Fatalf("zip listing made %d range requests", ra.Reads)
	}
}

func TestTar(t *testing.T) {
	data := makeTar(t, false)
	l, err := archive.List("tar", bytes.NewReader(data), int64(len(data)), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 6 || l.Truncated {
		t.Fatalf("tar %+v", l)
	}
}

func TestTgzBudget(t *testing.T) {
	data := makeTar(t, true)
	l, err := archive.List("tgz", bytes.NewReader(data), int64(len(data)), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 6 {
		t.Fatalf("tgz %+v", l)
	}
	l, err = archive.List("tgz", bytes.NewReader(data), int64(len(data)), 100)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Truncated {
		t.Fatal("expected truncation under a tiny budget")
	}
}

func TestCorrupt(t *testing.T) {
	junk := []byte("not an archive at all")
	if _, err := archive.List("zip", bytes.NewReader(junk), int64(len(junk)), 1<<20); err == nil {
		t.Fatal("corrupt zip accepted")
	}
	if _, err := archive.List("7z", bytes.NewReader(junk), int64(len(junk)), 1<<20); err == nil {
		t.Fatal("corrupt 7z accepted")
	}
	if _, err := archive.List("rar", bytes.NewReader(junk), int64(len(junk)), 1<<20); err == nil {
		t.Fatal("corrupt rar accepted")
	}
}

func TestFormat(t *testing.T) {
	for name, want := range map[string]string{"a.ZIP": "zip", "b.tar.gz": "tgz", "c.tgz": "tgz", "d.7z": "7z", "e.rar": "rar", "f.tar": "tar", "g.txt": ""} {
		if got := archive.Format(name); got != want {
			t.Errorf("%s: %s", name, got)
		}
	}
}
