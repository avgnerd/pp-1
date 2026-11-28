package backup

import (
	"encoding/json"
	"strings"
	"testing"
)

// A preview is built from an envelope and a payload that validation accepted, but the summary must
// stay safe when the payload is missing: a nil payload must render an empty storage list, not panic
// while the length is read before the nil check.
func TestPreviewSummaryNilPayloadReturnsEmptyStorageList(t *testing.T) {
	p := &Preview{
		ID:             "0123456789abcdef0123456789abcdef",
		FormatVersion:  1,
		Encrypted:      false,
		EncryptionMode: ModeNone,
		CreatedAt:      "2024-05-01T12:00:00Z",
		AppVersion:     "1.0.0",
		Payload:        nil,
	}
	summary := p.Summary()
	if summary == nil {
		t.Fatal("Summary() = nil")
	}
	if summary.Storages == nil {
		t.Fatal("Storages = nil, want a non-nil empty slice")
	}
	if len(summary.Storages) != 0 {
		t.Fatalf("storages = %+v, want empty", summary.Storages)
	}
	if summary.SettingsCount != 0 || summary.AdminUsername != "" {
		t.Fatalf("payload fields leaked from a nil payload: %+v", summary)
	}

	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"storages":[]`) {
		t.Fatalf("summary JSON storages = %s, want []", body)
	}
}
