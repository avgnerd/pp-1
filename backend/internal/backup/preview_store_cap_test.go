package backup

import (
	"errors"
	"testing"
	"time"
)

// The preview store must not grow without bound: each uploaded file is held in plaintext memory
// until it is applied or expires, so it keeps a small fixed maximum and evicts the previews that
// would expire first. 32 is the retained-preview limit the store promises.
func TestPreviewStoreRetainsAtMost32Previews(t *testing.T) {
	const maxRetained = 32
	store := NewPreviewStore()
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	env := &Envelope{FormatVersion: 1, EncryptionMode: ModeNone,
		CreatedAt: "2024-05-01T12:00:00Z", AppVersion: "1.0.0"}

	ids := make([]string, 0, maxRetained+1)
	payload := validPayload(t)
	for i := 0; i < maxRetained+1; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		store.now = func() time.Time { return at }
		pv, err := store.Put("session-a", env, payload)
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		ids = append(ids, pv.ID)
	}

	if got := store.Len(); got > maxRetained {
		t.Fatalf("retained %d previews, want at most %d", got, maxRetained)
	}
	// The soonest-expiring preview is the one evicted; the newest one is still usable.
	if _, err := store.Take(ids[0], "session-a"); !errors.Is(err, ErrPreviewInvalid) {
		t.Fatalf("oldest preview err = %v, want ErrPreviewInvalid", err)
	}
	if _, err := store.Take(ids[len(ids)-1], "session-a"); err != nil {
		t.Fatalf("newest preview was evicted: %v", err)
	}
}
