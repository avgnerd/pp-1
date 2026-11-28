package auth

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestLoginQueuedBeforeLockIssuesNoSession reproduces the concurrent-login lockout bypass. Eight
// correct-password logins pass the initial lock check and are held inside the password check while
// five wrong-password attempts lock the IP. Releasing them must not let any queued login clear the
// failure record or mint a session. The password-check seam is the barrier, so the ordering does
// not rely on a sleep or on scheduler luck.
func TestLoginQueuedBeforeLockIssuesNoSession(t *testing.T) {
	s, d := newService(t)
	const (
		queued = 8
		ip     = "198.51.100.20"
	)

	verified := make(chan struct{}, queued)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	s.verifyPassword = func(encoded, pw string) bool {
		if pw != DefaultPassword {
			return false
		}
		verified <- struct{}{}
		<-release
		return true
	}
	t.Cleanup(func() { s.verifyPassword = VerifyPassword })

	type result struct {
		token string
		err   error
	}
	results := make(chan result, queued)
	for i := 0; i < queued; i++ {
		go func() {
			tok, err := s.Login(DefaultUsername, DefaultPassword, ip, "test")
			results <- result{token: tok, err: err}
		}()
	}
	for i := 0; i < queued; i++ {
		select {
		case <-verified:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d logins reached the password check", i, queued)
		}
	}

	for i := 0; i < 5; i++ {
		_, err := s.Login(DefaultUsername, "wrong-password", ip, "test")
		switch {
		case i == 4 && !errors.Is(err, ErrLocked):
			t.Fatalf("failure %d err = %v, want ErrLocked", i+1, err)
		case i < 4 && !errors.Is(err, ErrInvalidCredentials):
			t.Fatalf("failure %d err = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	releaseAll()

	issued := 0
	for i := 0; i < queued; i++ {
		r := <-results
		if !errors.Is(r.err, ErrLocked) {
			t.Errorf("queued login %d err = %v, want ErrLocked", i, r.err)
		}
		if r.token != "" {
			issued++
		}
	}
	if issued != 0 {
		t.Fatalf("%d of %d queued logins issued a session after the IP was locked", issued, queued)
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("sessions table has %d rows after the IP locked, want 0", rows)
	}
	if !s.limiter.Locked(ip) {
		t.Fatal("a queued login cleared the lock while recording a success")
	}
}
