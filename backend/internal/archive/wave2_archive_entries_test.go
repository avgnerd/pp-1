package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/archive"
)

func wave2EmptyZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func wave2EmptyTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// An archive with no entries must still answer with a JSON array. A nil Entries slice marshals
// as null, and the preview then crashes iterating it instead of showing an empty archive.
func TestListEmptyArchivesMarshalEntriesAsArray(t *testing.T) {
	cases := []struct {
		name   string
		format string
		data   []byte
	}{
		{"zip", "zip", wave2EmptyZip(t)},
		{"tar", "tar", wave2EmptyTar(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, err := archive.List(tc.format, bytes.NewReader(tc.data), int64(len(tc.data)), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if l.Entries == nil {
				t.Fatal("Entries is nil; JSON will encode it as null")
			}
			if len(l.Entries) != 0 {
				t.Fatalf("entries = %+v, want none", l.Entries)
			}
			b, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(b); !strings.Contains(got, `"entries":[]`) || strings.Contains(got, `"entries":null`) {
				t.Fatalf("empty %s listing JSON = %s, want \"entries\":[]", tc.name, got)
			}
		})
	}
}
