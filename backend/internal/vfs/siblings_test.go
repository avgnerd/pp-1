package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/pathutil"
	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// Sibling paths are cleaned before taking their parent. A trailing slash names /m/sub, so its
// siblings are the files in /m, not the files inside /m/sub.
func TestSiblingFilesWithTrailingSlashUsesParent(t *testing.T) {
	svc, fs, _ := newTestService(t, "siblings-slash")
	acct := fs.Account("siblings-slash")
	mod := time.Unix(1700000000, 0).UTC()
	acct.Put("/sub/child.txt", []byte("child"), mod)
	acct.Put("/sibling.txt", []byte("sibling"), mod)

	got, err := svc.SiblingFiles(context.Background(), "/m/sub/", "", "")
	if err != nil {
		t.Fatalf("siblings: %v", err)
	}
	if len(got) != 1 || got[0].Name != "sibling.txt" {
		t.Fatalf("siblings of /m/sub/ = %+v, want only sibling.txt", got)
	}
}

// An invalid path returns the cleaning error. Resolving its parent first would make "/m/.." look
// like "/m" and a control character in the last segment disappear entirely.
func TestSiblingFilesRejectsUncleanPaths(t *testing.T) {
	svc, _, _ := newTestService(t, "siblings-invalid")

	cases := []struct {
		name string
		path string
		want string
	}{
		{"parent segment", "/m/..", "경로에 '..'를 사용할 수 없습니다"},
		{"control character", "/m/bad\x01name", "경로에 제어 문자를 사용할 수 없습니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SiblingFiles(context.Background(), tc.path, "", "")
			var pe *pathutil.PathError
			if !errors.As(err, &pe) {
				t.Fatalf("SiblingFiles(%q) error = %v, want *pathutil.PathError", tc.path, err)
			}
			if pe.Msg != tc.want {
				t.Fatalf("error = %q, want %q", pe.Msg, tc.want)
			}
		})
	}
}

// A folder whose only entries are folders has no sibling files, and that list must encode as [].
func TestSiblingFilesWithoutFilesReturnsEmptySlice(t *testing.T) {
	svc, fs, _ := newTestService(t, "siblings-empty")
	fs.Account("siblings-empty").Mkdir("/only")

	got, err := svc.SiblingFiles(context.Background(), "/m/only", "", "")
	if err != nil {
		t.Fatalf("siblings: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("siblings = %#v, want a non-nil empty slice", got)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "[]" {
		t.Fatalf("siblings JSON = %s, want []", body)
	}
}

// The virtual root with no storages has no children, and its entries must encode as [].
func TestVirtualRootWithoutStoragesSerializesEmptyEntries(t *testing.T) {
	svc := &Service{St: &storage.Manager{}}

	res, err := svc.List(context.Background(), "/", ListOptions{})
	if err != nil {
		t.Fatalf("list /: %v", err)
	}
	if res.Entries == nil {
		t.Fatal("entries = nil, want a non-nil empty slice")
	}
	body, err := json.Marshal(res.Entries)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "[]" {
		t.Fatalf("entries JSON = %s, want []", body)
	}
}
