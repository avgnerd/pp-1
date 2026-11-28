package backup

import (
	"errors"
	"testing"
	"time"
)

// putPreview registers one preview for session at the store's current clock.
func putPreview(t *testing.T, store *PreviewStore, session string, payload *Payload) *Preview {
	t.Helper()
	env := &Envelope{FormatVersion: 1, EncryptionMode: ModeNone,
		CreatedAt: "2024-05-01T12:00:00Z", AppVersion: "1.0.0"}
	pv, err := store.Put(session, env, payload)
	if err != nil {
		t.Fatalf("put for %s: %v", session, err)
	}
	return pv
}

// Purge drops every preview past its ten-minute window and reports how many it removed. The same
// expiry must also make the preview unusable even if Purge has not run yet.
func TestPreviewStorePurgeRemovesExpired(t *testing.T) {
	store := NewPreviewStore()
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	payload := validPayload(t)
	first := putPreview(t, store, "session-a", payload)
	second := putPreview(t, store, "session-b", payload)
	if got := store.Len(); got != 2 {
		t.Fatalf("previews = %d, want 2", got)
	}
	if removed := store.Purge(); removed != 0 {
		t.Fatalf("Purge before expiry removed %d, want 0", removed)
	}

	store.now = func() time.Time { return base.Add(PreviewTTL + time.Second) }
	if removed := store.Purge(); removed != 2 {
		t.Fatalf("Purge removed %d expired previews, want 2", removed)
	}
	if got := store.Len(); got != 0 {
		t.Fatalf("previews after Purge = %d, want 0", got)
	}
	for _, pv := range []*Preview{first, second} {
		if _, err := store.Take(pv.ID, pv.Session); !errors.Is(err, ErrPreviewInvalid) {
			t.Fatalf("expired preview %s err = %v, want ErrPreviewInvalid", pv.ID, err)
		}
	}
	if removed := store.Purge(); removed != 0 {
		t.Fatalf("Purge on an empty store removed %d, want 0", removed)
	}
}

// A logout may only remove the previews issued to that login session. Another admin session's
// preview must survive, because the id is bound to that session and could still be applied there.
func TestPreviewStoreDropSessionOnlyAffectsThatSession(t *testing.T) {
	store := NewPreviewStore()
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	payload := validPayload(t)
	first := putPreview(t, store, "session-a", payload)
	second := putPreview(t, store, "session-a", payload)
	other := putPreview(t, store, "session-b", payload)

	if removed := store.DropSession("session-a"); removed != 2 {
		t.Fatalf("DropSession(session-a) = %d, want 2", removed)
	}
	if got := store.Len(); got != 1 {
		t.Fatalf("previews after drop = %d, want 1", got)
	}
	for _, pv := range []*Preview{first, second} {
		if _, err := store.Take(pv.ID, "session-a"); !errors.Is(err, ErrPreviewInvalid) {
			t.Fatalf("preview of dropped session err = %v, want ErrPreviewInvalid", err)
		}
	}
	if _, err := store.Take(other.ID, "session-b"); err != nil {
		t.Fatalf("other session's preview was dropped: %v", err)
	}
	if removed := store.DropSession("session-a"); removed != 0 {
		t.Fatalf("second DropSession = %d, want 0", removed)
	}
}

// Clear drops every preview, including ones from other sessions: a successful restore revokes all
// sessions, so no preview bound to them may keep a plaintext payload alive.
func TestPreviewStoreClearRemovesEverything(t *testing.T) {
	store := NewPreviewStore()
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return base }
	payload := validPayload(t)
	first := putPreview(t, store, "session-a", payload)
	second := putPreview(t, store, "session-b", payload)

	store.Clear()
	if got := store.Len(); got != 0 {
		t.Fatalf("previews after Clear = %d, want 0", got)
	}
	for _, pv := range []*Preview{first, second} {
		if _, err := store.Take(pv.ID, pv.Session); !errors.Is(err, ErrPreviewInvalid) {
			t.Fatalf("preview after Clear err = %v, want ErrPreviewInvalid", err)
		}
	}
	store.Clear() // idempotent, no panic
}
