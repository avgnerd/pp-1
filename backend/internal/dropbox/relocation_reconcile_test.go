package dropbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
)

// TestReconcileMoveUsesDestinationIdentity proves a moved file is recognized by the destination's
// stable id, not by the source having disappeared. The same helper reports a move that landed
// somewhere else as unknown rather than success.
func TestReconcileMoveUsesDestinationIdentity(t *testing.T) {
	fs, c := setup(t)
	a := fs.Account("app1")
	a.Put("/move.txt", []byte("move"), time.Now())
	a.Mkdir("/dest")
	src, err := c.GetMetadata(context.Background(), "/move.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Move(context.Background(), "/move.txt", "/dest/move.txt"); err != nil {
		t.Fatal(err)
	}
	out := c.ReconcileRelocation(context.Background(), dropbox.Relocation{
		Op: dropbox.OpMove, From: "/move.txt", To: "/dest/move.txt", Name: "move.txt", SourceID: src.ID,
	})
	if out != dropbox.RelocationApplied {
		t.Fatalf("move outcome = %v, want applied from the destination id", out)
	}

	a.Put("/elsewhere.txt", []byte("other"), time.Now())
	other, err := c.GetMetadata(context.Background(), "/elsewhere.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Move(context.Background(), "/elsewhere.txt", "/moved-away.txt"); err != nil {
		t.Fatal(err)
	}
	out = c.ReconcileRelocation(context.Background(), dropbox.Relocation{
		Op: dropbox.OpMove, From: "/elsewhere.txt", To: "/dest/elsewhere.txt", Name: "elsewhere.txt", SourceID: other.ID,
	})
	if out != dropbox.RelocationUnknown {
		t.Fatalf("a move to another destination reported %v, want unknown", out)
	}
}

// TestReconcileCopyRequiresPreWriteDestinationState verifies that identical content alone never
// proves a copy: a destination that existed before is a genuine conflict, and a copy without
// pre-write destination evidence stays unknown.
func TestReconcileCopyRequiresPreWriteDestinationState(t *testing.T) {
	fs, c := setup(t)
	a := fs.Account("app1")
	a.Put("/src.txt", []byte("identical"), time.Now())
	a.Mkdir("/dest")
	a.Put("/dest/src.txt", []byte("identical"), time.Now())
	src, err := c.GetMetadata(context.Background(), "/src.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Copy(context.Background(), "/src.txt", "/dest/src.txt"); err == nil {
		t.Fatal("copying onto an existing destination succeeded")
	}
	reloc := dropbox.Relocation{
		Op: dropbox.OpCopy, From: "/src.txt", To: "/dest/src.txt", Name: "src.txt",
		SourceID: src.ID, SourceRev: src.Rev, DestChecked: true, DestExisted: true,
	}
	if out := c.ReconcileRelocation(context.Background(), reloc); out != dropbox.RelocationFailed {
		t.Fatalf("same-content pre-existing destination reported %v, want failed", out)
	}
	reloc.DestExisted, reloc.DestChecked = false, false
	if out := c.ReconcileRelocation(context.Background(), reloc); out != dropbox.RelocationUnknown {
		t.Fatalf("a copy without destination evidence reported %v, want unknown", out)
	}
}

// TestReconcileCopyConfirmsOnlyAbsentThenMatchingDestination covers the true-success direction: the
// destination was inspected and absent before the write, then holds the unchanged source content.
func TestReconcileCopyConfirmsOnlyAbsentThenMatchingDestination(t *testing.T) {
	fs, c := setup(t)
	a := fs.Account("app1")
	a.Put("/src.txt", []byte("copy me"), time.Now())
	a.Mkdir("/dest")
	src, err := c.GetMetadata(context.Background(), "/src.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetMetadata(context.Background(), "/dest/src.txt"); err == nil {
		t.Fatal("the destination unexpectedly existed before the copy")
	}
	if err := c.Copy(context.Background(), "/src.txt", "/dest/src.txt"); err != nil {
		t.Fatal(err)
	}
	out := c.ReconcileRelocation(context.Background(), dropbox.Relocation{
		Op: dropbox.OpCopy, From: "/src.txt", To: "/dest/src.txt", Name: "src.txt",
		SourceID: src.ID, SourceRev: src.Rev, DestChecked: true,
	})
	if out != dropbox.RelocationApplied {
		t.Fatalf("copy outcome = %v, want applied from the pre-write absent destination", out)
	}
}

// TestReconcileDeleteUsesMetadata checks that a delete after a lost response is success only when
// the entry is actually gone, and a failure while it is still present.
func TestReconcileDeleteUsesMetadata(t *testing.T) {
	fs, c := setup(t)
	a := fs.Account("app1")
	a.Put("/del.txt", []byte("bye"), time.Now())
	a.Put("/keep.txt", []byte("stay"), time.Now())
	if err := c.Delete(context.Background(), "/del.txt"); err != nil {
		t.Fatal(err)
	}
	if out := c.ReconcileDelete(context.Background(), "/del.txt"); out != dropbox.RelocationApplied {
		t.Fatalf("deleted entry reported %v, want applied", out)
	}
	if out := c.ReconcileDelete(context.Background(), "/keep.txt"); out != dropbox.RelocationFailed {
		t.Fatalf("present entry reported %v, want failed", out)
	}
}
