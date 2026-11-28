package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/archive"
)

func wave2Tar(t *testing.T, headers []tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := range headers {
		h := headers[i]
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tar -czf archive.tgz . stores every name as "./...". The explicit "./src/" folder and the
// folder implied by "./src/main.go" are the same folder, so the listing must carry exactly one
// "src/" entry and no name that still starts with "./".
func TestTarDotSlashNamesDoNotDuplicateImplicitDirs(t *testing.T) {
	now := time.Now()
	src := []byte("package main")
	data := wave2Tar(t,
		[]tar.Header{
			{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: now},
			{Name: "./src/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: now},
			{Name: "./src/main.go", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(src)), ModTime: now},
		},
		[][]byte{nil, nil, src})

	l, err := archive.List("tar", bytes.NewReader(data), int64(len(data)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleSrcTree(t, l)
}

func TestTarDotSlashFileNameNormalizesBeforeImplicitDirs(t *testing.T) {
	src := []byte("package main")
	data := wave2Tar(t,
		[]tar.Header{{Name: "./src/main.go", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(src)), ModTime: time.Now()}},
		[][]byte{src})

	l, err := archive.List("tar", bytes.NewReader(data), int64(len(data)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleSrcTree(t, l)
}

func assertSingleSrcTree(t *testing.T, l *archive.Listing) {
	t.Helper()
	dirs := 0
	files := 0
	for _, e := range l.Entries {
		if strings.HasPrefix(e.Name, "./") {
			t.Fatalf("entry %q still has a leading ./", e.Name)
		}
		switch e.Name {
		case "src/":
			dirs++
			if !e.Dir {
				t.Fatalf("src/ entry is not a directory: %+v", e)
			}
		case "src/main.go":
			files++
		default:
			t.Fatalf("unexpected entry %+v", e)
		}
	}
	if dirs != 1 || files != 1 {
		t.Fatalf("entries = %+v, want exactly one src/ directory and one src/main.go file", l.Entries)
	}
}

// A ZIP written on Windows can use "\" as the separator, including on the folder entry itself.
// The entry is a directory, not a zero-byte file named "folder/".
func TestZipBackslashFolderEntryIsDirectory(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.CreateHeader(&zip.FileHeader{Name: `folder\`, Modified: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: `folder\a.txt`, Modified: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()

	l, err := archive.List("zip", bytes.NewReader(data), int64(len(data)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var dir, file *archive.Entry
	for i := range l.Entries {
		switch l.Entries[i].Name {
		case "folder/":
			dir = &l.Entries[i]
		case "folder/a.txt":
			file = &l.Entries[i]
		}
	}
	if dir == nil {
		t.Fatalf("entries = %+v, want folder/", l.Entries)
	}
	if !dir.Dir {
		t.Fatalf("folder/ entry = %+v, want Dir true (a zero-byte file with a trailing slash is a directory)", *dir)
	}
	if file == nil || file.Dir || file.Size != int64(len("hello")) {
		t.Fatalf("folder/a.txt entry = %+v, want a 5-byte file", file)
	}
}
