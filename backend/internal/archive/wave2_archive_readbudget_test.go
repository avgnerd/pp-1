package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"testing"
	"time"
)

// wave2Counting wraps a reader to count how many bytes the budget actually pulled from it.
type wave2Counting struct {
	r    *bytes.Reader
	read int
}

func (c *wave2Counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func wave2BudgetBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

// A stream that ends exactly at the budget must look like a clean EOF, not truncation.
func TestReadBudgetAllowsEOFExactlyAtBudget(t *testing.T) {
	const budget = 64
	src := &wave2Counting{r: bytes.NewReader(wave2BudgetBytes(budget))}
	b := &readBudget{r: src, max: budget}

	got, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("reader whose stream ends exactly at the %d-byte budget returned %v; want a clean io.EOF", budget, err)
	}
	if len(got) != budget {
		t.Fatalf("read %d bytes, want %d", len(got), budget)
	}
	if src.read != budget {
		t.Fatalf("underlying reader yielded %d bytes, want %d", src.read, budget)
	}
	if n, err := b.Read(make([]byte, 8)); n != 0 || err != io.EOF {
		t.Fatalf("read after the clean EOF = (%d, %v), want (0, io.EOF)", n, err)
	}
	if src.read != budget {
		t.Fatalf("underlying reader yielded %d bytes after a repeated read, want %d", src.read, budget)
	}
}

// A stream longer than the budget must still report ErrBudget, with the probe byte discarded
// and no more than that single byte read past the budget.
func TestReadBudgetProbeStopsOneBytePastBudget(t *testing.T) {
	const budget = 64
	all := wave2BudgetBytes(budget + 1)
	src := &wave2Counting{r: bytes.NewReader(all)}
	b := &readBudget{r: src, max: budget}

	got, err := io.ReadAll(b)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("reader with one byte past the %d-byte budget returned %v; want ErrBudget", budget, err)
	}
	if !bytes.Equal(got, all[:budget]) {
		t.Fatalf("read %q, want the first %d bytes only", got, budget)
	}
	if src.read != budget+1 {
		t.Fatalf("underlying reader yielded %d bytes, want exactly %d (one probe byte past the budget)", src.read, budget+1)
	}
	if n, err := b.Read(make([]byte, 8)); n != 0 || !errors.Is(err, ErrBudget) {
		t.Fatalf("read after ErrBudget = (%d, %v), want (0, ErrBudget)", n, err)
	}
	if src.read != budget+1 {
		t.Fatalf("underlying reader yielded %d bytes after a repeated read, want exactly %d", src.read, budget+1)
	}
}

// wave2OneZeroBlockTgz builds a TAR whose decompressed size is exactly budget bytes: one
// 512-byte file header, the file body, and a single zero trailer block. Go's tar reader
// accepts one zero block followed by EOF.
func wave2OneZeroBlockTgz(t *testing.T, budget int64) []byte {
	t.Helper()
	const block = 512
	body := make([]byte, budget-2*block)
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	hdr := &tar.Header{Name: "exact.bin", Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(0, 0)}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	// tw.Close would write two zero blocks; this fixture needs exactly one.
	if int64(raw.Len()) != budget-block {
		t.Fatalf("raw tar = %d bytes before the trailer, want %d", raw.Len(), budget-block)
	}
	raw.Write(make([]byte, block))

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if int64(raw.Len()) != budget {
		t.Fatalf("decompressed tar = %d bytes, want exactly %d", raw.Len(), budget)
	}
	return gz.Bytes()
}

// A complete TAR that ends exactly at the budget, with only a one-block trailer, must not be
// marked truncated just because the tar reader needs one more EOF probe.
func TestTgzOneZeroBlockTrailerAtExactBudgetIsNotTruncated(t *testing.T) {
	const budget = 1 << 20
	data := wave2OneZeroBlockTgz(t, budget)
	if int64(len(data)) >= budget {
		t.Fatalf("fixture compressed size %d is not below the %d budget", len(data), budget)
	}

	l, err := List("tgz", bytes.NewReader(data), int64(len(data)), budget)
	if err != nil {
		t.Fatalf("listing a tgz that ends exactly at the %d-byte budget returned error: %v", budget, err)
	}
	if l.Truncated {
		t.Fatalf("Truncated = true: a complete listing whose decompressed stream ends exactly at the %d-byte budget was reported truncated (%+v)", budget, l.Entries)
	}
	if len(l.Entries) != 1 || l.Entries[0].Name != "exact.bin" || l.Entries[0].Size != budget-2*512 {
		t.Fatalf("entries = %+v, want exact.bin of %d bytes", l.Entries, budget-2*512)
	}
}
