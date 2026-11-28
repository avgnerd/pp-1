package dropbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
)

// TestReconcileMoveCaseOnlyRenameIsFailed models a case-only rename that did not happen. The fake
// resolves metadata case-insensitively, as Dropbox does, so /Foo.txt returns the still-present
// /foo.txt entry with the source's stable id. That is the source, not a completed rename.
func TestReconcileMoveCaseOnlyRenameIsFailed(t *testing.T) {
	fs, c := setup(t)
	a := fs.Account("app1")
	a.Put("/foo.txt", []byte("case"), time.Now())
	src, err := c.GetMetadata(context.Background(), "/foo.txt")
	if err != nil {
		t.Fatal(err)
	}

	out := c.ReconcileRelocation(context.Background(), dropbox.Relocation{
		Op:       dropbox.OpMove,
		From:     "/foo.txt",
		To:       "/Foo.txt",
		Name:     "Foo.txt",
		SourceID: src.ID,
	})
	if out != dropbox.RelocationFailed {
		t.Fatalf("case-only rename that left /foo.txt in place reported %v, want RelocationFailed", out)
	}
}
