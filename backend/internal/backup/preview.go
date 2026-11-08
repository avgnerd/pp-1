package backup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// PreviewTTL is how long a preview_id stays usable after the preview was produced. The restore
// requirements give the admin ten minutes to review a preview.
var PreviewTTL = 10 * time.Minute

// ErrPreviewInvalid covers every reason a preview_id cannot be applied: unknown, already used,
// expired, or bound to another login session. The cases are deliberately not distinguished so a
// caller cannot probe for ids that exist.
var ErrPreviewInvalid = errors.New("미리보기 식별자가 없거나 만료되었습니다. 백업 파일을 다시 올려 주세요")

// PreviewStorage is one row of the preview's storage summary. It never carries credentials.
type PreviewStorage struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	Status    string `json:"status"`
}

// RestorePreview is the JSON body the admin UI shows before a restore is applied.
type RestorePreview struct {
	PreviewID      string           `json:"preview_id"`
	FormatVersion  int              `json:"format_version"`
	Encrypted      bool             `json:"encrypted"`
	EncryptionMode string           `json:"encryption_mode"`
	CreatedAt      string           `json:"created_at"`
	AppVersion     string           `json:"app_version"`
	AdminUsername  string           `json:"admin_username"`
	SettingsCount  int              `json:"settings_count"`
	Storages       []PreviewStorage `json:"storages"`
}

// Preview is one validated backup waiting for the admin to confirm it.
type Preview struct {
	ID             string
	Session        string
	FormatVersion  int
	Encrypted      bool
	EncryptionMode string
	CreatedAt      string
	AppVersion     string
	Payload        *Payload
	Expires        time.Time
}

// Summary renders the client-facing view of the preview. Secrets stay behind: only names, mount
// paths and counts leave the server.
func (p *Preview) Summary() *RestorePreview {
	out := &RestorePreview{
		PreviewID:      p.ID,
		FormatVersion:  p.FormatVersion,
		Encrypted:      p.Encrypted,
		EncryptionMode: p.EncryptionMode,
		CreatedAt:      p.CreatedAt,
		AppVersion:     p.AppVersion,
		Storages:       make([]PreviewStorage, 0, len(p.Payload.Storages)),
	}
	if p.Payload != nil {
		out.AdminUsername = p.Payload.Admin.Username
		out.SettingsCount = len(p.Payload.Settings)
		for _, s := range p.Payload.Storages {
			out.Storages = append(out.Storages, PreviewStorage{Name: s.Name, MountPath: s.MountPath, Status: s.Status})
		}
	}
	return out
}

// PreviewStore keeps validated previews in memory, bound to the login session that created them.
type PreviewStore struct {
	mu  sync.Mutex
	m   map[string]*Preview
	ttl time.Duration
	now func() time.Time
}

// NewPreviewStore returns an empty store.
func NewPreviewStore() *PreviewStore {
	return &PreviewStore{m: map[string]*Preview{}, ttl: PreviewTTL, now: time.Now}
}

// Put registers a validated payload and returns the preview, including its fresh preview_id.
func (s *PreviewStore) Put(session string, env *Envelope, payload *Payload) (*Preview, error) {
	if env == nil || payload == nil {
		return nil, &ValidationError{Msg: "미리보기를 만들 수 없는 백업입니다"}
	}
	id, err := newPreviewID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	p := &Preview{
		ID:             id,
		Session:        session,
		FormatVersion:  env.FormatVersion,
		Encrypted:      env.Encrypted,
		EncryptionMode: env.EncryptionMode,
		CreatedAt:      env.CreatedAt,
		AppVersion:     env.AppVersion,
		Payload:        payload,
		Expires:        now.Add(s.ttl),
	}
	s.mu.Lock()
	for k, v := range s.m {
		if now.After(v.Expires) {
			delete(s.m, k)
		}
	}
	s.m[p.ID] = p
	s.mu.Unlock()
	return p, nil
}

// Take consumes a preview_id: after this call the id can never be applied again. The id must have
// been issued to the same login session and must not have expired.
func (s *PreviewStore) Take(id, session string) (*Preview, error) {
	if id == "" {
		return nil, ErrPreviewInvalid
	}
	now := s.now()
	s.mu.Lock()
	p, ok := s.m[id]
	if ok && now.After(p.Expires) {
		delete(s.m, id)
		ok = false
	}
	if ok && p.Session != session {
		// Do not delete: another session must not be able to invalidate this preview.
		s.mu.Unlock()
		return nil, ErrPreviewInvalid
	}
	if ok {
		delete(s.m, id)
	}
	s.mu.Unlock()
	if !ok {
		return nil, ErrPreviewInvalid
	}
	return p, nil
}

// Len reports how many previews are stored (tests and diagnostics).
func (s *PreviewStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// newPreviewID returns 16 random bytes as 32 hex characters.
func newPreviewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("미리보기 식별자를 만들지 못했습니다")
	}
	return hex.EncodeToString(b), nil
}
