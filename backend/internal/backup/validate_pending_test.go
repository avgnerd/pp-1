package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/storage"
)

// TestValidatePayloadRequiresPendingCredentials pins that a pending storage is held to the
// credentials storage.Create stores for it. Create always writes an app key and an app secret,
// even for a pending row, and Update cannot replace either: a restored pending storage missing one
// has no way back to a working authorization.
func TestValidatePayloadRequiresPendingCredentials(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*StorageRecord)
		wantSub string
	}{
		{"pending without app key", func(r *StorageRecord) { r.AppKey = "" }, "app_key"},
		{"pending without app secret", func(r *StorageRecord) { r.AppSecret = "" }, "app_secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPayload(t)
			std := p.Storages[1]
			if std.Status != storage.StatusPending {
				t.Fatalf("fixture storage is %q, want a pending one", std.Status)
			}
			tc.mutate(&p.Storages[1])
			err := ValidatePayload(p)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidatePayload = %v, want a ValidationError", err)
			}
			if !strings.Contains(ve.Msg, tc.wantSub) {
				t.Fatalf("message %q does not mention %q", ve.Msg, tc.wantSub)
			}
		})
	}
}

// TestValidatePayloadAcceptsPendingWithoutRefreshToken is the other half of the rule: Create writes
// no refresh token for a pending storage, the OAuth completion writes it. Rejecting its absence
// would refuse every backup taken while a storage waits for its authorization.
func TestValidatePayloadAcceptsPendingWithoutRefreshToken(t *testing.T) {
	p := validPayload(t)
	p.Storages[1].RefreshToken = ""
	if err := ValidatePayload(p); err != nil {
		t.Fatalf("pending storage without a refresh token rejected: %v", err)
	}
}

// TestExportOfPendingStoragePassesValidation is the compatibility contract for backups this build
// writes itself: a pending storage created the way storage.Create creates one exports with the app
// key and app secret present, so the tighter rule cannot refuse the app's own backup.
func TestExportOfPendingStoragePassesValidation(t *testing.T) {
	f := newFixture(t)
	f.seedStorage(3, "Pending", "/pending", "pending", "app-key-3", "app-secret-3", "")
	env := f.export(ModeNone, "", true)
	_, payload := openFile(t, env, "", f.key)
	var pending *StorageRecord
	for i := range payload.Storages {
		if payload.Storages[i].Status == storage.StatusPending {
			pending = &payload.Storages[i]
		}
	}
	if pending == nil {
		t.Fatalf("export dropped the pending storage: %+v", payload.Storages)
	}
	if pending.AppKey == "" || pending.AppSecret == "" {
		t.Fatalf("exported pending storage = %+v, want both credentials", *pending)
	}
	if pending.RefreshToken != "" {
		t.Fatalf("exported pending storage carries a refresh token %q", pending.RefreshToken)
	}
	if err := ValidatePayload(payload); err != nil {
		t.Fatalf("the app's own backup of a pending storage is rejected: %v", err)
	}
}

// TestRestoredPendingStorageKeepsAnUnusableSecret shows what the rule prevents. A payload whose
// pending storage carries no app secret is stored with an empty app_secret_enc column, and the
// OAuth completion opens exactly that column (storage.Manager.complete), so the storage can never
// be authorized: the admin can only delete it and create it again.
func TestRestoredPendingStorageKeepsAnUnusableSecret(t *testing.T) {
	f := newFixture(t)
	p := validPayload(t)
	p.Storages = []StorageRecord{{ID: 2, Name: "Pending", MountPath: "/pending",
		Status: storage.StatusPending, AppKey: "app-key", AppSecret: ""}}

	if err := ApplyTx(context.Background(), f.db, p, ApplyOptions{Box: f.box}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var sealed []byte
	if err := f.db.QueryRow(`SELECT app_secret_enc FROM storages WHERE id = 2`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if len(sealed) != 0 {
		t.Fatalf("app_secret_enc = %x, want the empty blob a missing secret seals to", sealed)
	}
	if _, err := f.box.Open(sealed); err == nil {
		t.Fatal("an empty sealed secret opened; storage.Manager.complete would have exchanged a token")
	}
}
