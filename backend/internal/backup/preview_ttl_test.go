package backup

import (
	"errors"
	"testing"
	"time"
)

// The restore requirements fix the preview window at ten minutes, and the store must enforce
// exactly that window. The clock is injected, so both boundaries are checked without waiting.
func TestPreviewTTLMatchesRequirement(t *testing.T) {
	if PreviewTTL != 10*time.Minute {
		t.Fatalf("PreviewTTL = %v, want the 10 minutes the restore requirements state", PreviewTTL)
	}
	store := NewPreviewStore()
	if store.ttl != PreviewTTL {
		t.Fatalf("new store ttl = %v, want PreviewTTL %v", store.ttl, PreviewTTL)
	}

	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	env := &Envelope{FormatVersion: 1, EncryptionMode: ModeNone,
		CreatedAt: "2024-05-01T12:00:00Z", AppVersion: "1.0.0"}
	store.now = func() time.Time { return base }
	beforeDeadline, err := store.Put("session-a", env, validPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(10*time.Minute - time.Second) }
	if _, err := store.Take(beforeDeadline.ID, "session-a"); err != nil {
		t.Fatalf("a preview must stay usable just before ten minutes: %v", err)
	}

	store.now = func() time.Time { return base }
	afterDeadline, err := store.Put("session-a", env, validPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(10*time.Minute + time.Second) }
	if _, err := store.Take(afterDeadline.ID, "session-a"); !errors.Is(err, ErrPreviewInvalid) {
		t.Fatalf("a preview must be expired one second past ten minutes, got %v", err)
	}
}
