package server

import (
	"log/slog"
	"time"
)

// auditLogger writes one structured line per privileged operation (backup export, restore
// preview, snapshot preview, restore apply).
//
// Only metadata may be passed to Audit: an action name, an outcome, a failure category, the
// encryption mode, a storage count and the request IP. Backup passwords, password hashes, app
// secrets, refresh tokens, ciphertext and the instance master key must never reach it.
type auditLogger struct{ log *slog.Logger }

// newAuditLogger wraps l (slog.Default when nil) as the audit sink.
func newAuditLogger(l *slog.Logger) *auditLogger {
	if l == nil {
		l = slog.Default()
	}
	return &auditLogger{log: l}
}

// Audit records one event. attrs must be alternating keys and values.
func (a *auditLogger) Audit(action string, attrs ...any) {
	if a == nil || a.log == nil {
		return
	}
	kv := make([]any, 0, len(attrs)+4)
	kv = append(kv, "audit", action, "at", time.Now().UTC().Format(time.RFC3339))
	kv = append(kv, attrs...)
	a.log.Info("audit", kv...)
}
