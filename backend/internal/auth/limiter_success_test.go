package auth

import (
	"testing"
	"time"
)

// TestAllowSuccessDoesNotClearLockedIP pins the atomic success gate: a locked IP is rejected and
// keeps its lock. With a separate check and Reset, a concurrent Fail could set lockedUntil between
// them and the Reset would then erase the lock while the session was issued.
func TestAllowSuccessDoesNotClearLockedIP(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	base := time.Now()
	l.now = func() time.Time { return base }
	const ip = "198.51.100.30"
	for i := 0; i < 5; i++ {
		l.Fail(ip)
	}
	if l.AllowSuccess(ip) {
		t.Fatal("AllowSuccess allowed a locked IP")
	}
	if !l.Locked(ip) {
		t.Fatal("AllowSuccess cleared the lock a concurrent failure would have set")
	}
	l.now = func() time.Time { return base.Add(time.Minute + time.Second) }
	if !l.AllowSuccess(ip) {
		t.Fatal("AllowSuccess rejected an IP whose lock had expired")
	}
	if l.Locked(ip) {
		t.Fatal("an expired lock survived a successful login")
	}
}

// TestAllowSuccessClearsUnlockedFailures requires a successful login to clear the failure record,
// so the next failure starts a fresh count instead of adding to the failures before the login.
func TestAllowSuccessClearsUnlockedFailures(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	base := time.Now()
	l.now = func() time.Time { return base }
	const ip = "198.51.100.31"
	for i := 0; i < 4; i++ {
		if l.Fail(ip) {
			t.Fatalf("failure %d locked the IP, want an unlocked IP", i+1)
		}
	}
	if !l.AllowSuccess(ip) {
		t.Fatal("AllowSuccess rejected an unlocked IP")
	}
	for i := 0; i < 4; i++ {
		if l.Fail(ip) {
			t.Fatalf("failure %d after a success locked the IP, want a fresh count", i+1)
		}
	}
}
