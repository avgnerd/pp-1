package archive_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/archive"
)

func wave2Tgz(t *testing.T, entries []tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for i := range entries {
		h := entries[i]
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
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func wave2CompressedTgz(t *testing.T) []byte {
	t.Helper()
	big := make([]byte, 8<<20) // zeros compress to a few KB
	now := time.Now()
	return wave2Tgz(t,
		[]tar.Header{
			{Name: "big.bin", Mode: 0o644, Size: int64(len(big)), ModTime: now},
			{Name: "after.txt", Mode: 0o644, Size: 5, ModTime: now},
		},
		[][]byte{big, []byte("after")})
}

// The gzip source is tiny, but the decompressed stream is much larger than the budget. The
// listing must stop at the decompressed budget and report truncation, not decompress the whole
// archive (and not turn the cap into a parse error).
func TestTgzDecompressedBudgetTruncatesHighlyCompressedArchive(t *testing.T) {
	data := wave2CompressedTgz(t)
	const budget = 1 << 20
	if int64(len(data)) >= budget {
		t.Fatalf("fixture compressed size %d is not below the %d budget", len(data), budget)
	}

	l, err := archive.List("tgz", bytes.NewReader(data), int64(len(data)), budget)
	if err != nil {
		t.Fatalf("listing a tgz whose decompressed size exceeds the budget returned error: %v", err)
	}
	if !l.Truncated {
		t.Fatalf("Truncated = false: %d compressed bytes expanding past %d decompressed bytes were listed as complete (%+v)",
			len(data), budget, l.Entries)
	}
}

// A budget that covers the decompressed stream must still list every entry.
func TestTgzDecompressedBudgetListsAllEntriesWhenLargeEnough(t *testing.T) {
	data := wave2CompressedTgz(t)
	const budget = 16 << 20

	l, err := archive.List("tgz", bytes.NewReader(data), int64(len(data)), budget)
	if err != nil {
		t.Fatal(err)
	}
	if l.Truncated {
		t.Fatalf("Truncated = true under a %d budget for %d decompressed bytes", budget, 8<<20+len("after"))
	}
	if len(l.Entries) != 2 {
		t.Fatalf("entries = %+v, want big.bin and after.txt", l.Entries)
	}
	names := map[string]bool{}
	for _, e := range l.Entries {
		names[e.Name] = true
	}
	if !names["big.bin"] || !names["after.txt"] {
		t.Fatalf("entries = %+v, want big.bin and after.txt", l.Entries)
	}
}
