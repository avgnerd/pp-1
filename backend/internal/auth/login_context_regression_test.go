package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestLoginContextStopsWaitingForKDFSlot covers a client that disconnects while queued behind the
// single KDF slot: the login must return its context error promptly instead of waiting for the
// running hash, and it must leave no session behind. A context that is already cancelled must not
// enter the queue at all.
func TestLoginContextStopsWaitingForKDFSlot(t *testing.T) {
	s, d := newService(t)
	const ip = "198.51.100.21"

	held := make(chan struct{})
	release := make(chan struct{})
	go runKDF(func() {
		close(held)
		<-release
	})
	<-held
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := s.LoginContext(ctx, DefaultUsername, DefaultPassword, ip, "test")
		done <- result{token: tok, err: err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("LoginContext err = %v, want context.DeadlineExceeded", r.err)
		}
		if r.token != "" {
			t.Fatalf("LoginContext issued token %q after its context expired", r.token)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoginContext did not stop waiting for the KDF slot after its context expired")
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	tok, err := s.LoginContext(cancelled, DefaultUsername, DefaultPassword, ip, "test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LoginContext with a cancelled context err = %v, want context.Canceled", err)
	}
	if tok != "" {
		t.Fatalf("LoginContext issued token %q for a cancelled context", tok)
	}

	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("sessions table has %d rows after cancelled logins, want 0", rows)
	}
	s.limiter.mu.Lock()
	failures := 0
	if a := s.limiter.m[ip]; a != nil {
		failures = a.count
	}
	s.limiter.mu.Unlock()
	if failures != 0 || s.limiter.Locked(ip) {
		t.Fatalf("logins cancelled before their KDF ran recorded %d failures and locked=%v, want 0 and false",
			failures, s.limiter.Locked(ip))
	}
}

// TestLoginWaitingForKDFSlotRejectsLockedIP drives the production verifier: each login passes the
// initial lock check, then waits behind the single KDF slot while the IP locks. The limiter clock
// signals each initial check, so no queued login is still ahead of that check when the lock lands.
// None of them may mint a session, and the limiter must stay locked.
func TestLoginWaitingForKDFSlotRejectsLockedIP(t *testing.T) {
	s, d := newService(t)
	const (
		queued = 4
		ip     = "198.51.100.22"
	)

	checked := make(chan struct{}, 64)
	s.limiter.now = func() time.Time {
		select {
		case checked <- struct{}{}:
		default:
		}
		return time.Now()
	}
	t.Cleanup(func() { s.limiter.now = time.Now })
	// Locked consults the clock only when the IP has an entry, so seed one to observe the check.
	s.limiter.mu.Lock()
	s.limiter.m[ip] = &attempts{first: time.Now()}
	s.limiter.mu.Unlock()

	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	go runKDF(func() {
		close(held)
		<-release
	})
	<-held

	type result struct {
		token string
		err   error
	}
	results := make(chan result, queued)
	for i := 0; i < queued; i++ {
		go func() {
			tok, err := s.LoginContext(context.Background(), DefaultUsername, DefaultPassword, ip, "test")
			results <- result{token: tok, err: err}
		}()
	}
	for i := 0; i < queued; i++ {
		select {
		case <-checked:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d logins passed the initial lock check", i, queued)
		}
	}
	for i := 0; i < 5; i++ {
		s.limiter.Fail(ip)
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
		t.Fatalf("%d of %d queued logins issued a session after the IP locked", issued, queued)
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

// TestRunKDFContextSkipsKDFAfterAdmissionRecheck pins the helper contract Login relies on: the
// admission callback runs while a slot is held, and its error skips the KDF body.
func TestRunKDFContextSkipsKDFAfterAdmissionRecheck(t *testing.T) {
	ran := false
	err := runKDFContext(context.Background(), func() error { return ErrLocked }, func() { ran = true })
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("runKDFContext err = %v, want ErrLocked", err)
	}
	if ran {
		t.Fatal("runKDFContext ran the KDF after the admission callback rejected the attempt")
	}
}

// TestLoginContextCountsCanceledWrongPasswordFailure reproduces the lockout-accounting bypass: a
// wrong-password login whose client disconnects while the password check runs must still count as a
// failure. The password-check seam holds each attempt open, so the cancellation is deterministic;
// five such attempts must leave the IP locked without any of them seeing a credential error or
// receiving a session.
func TestLoginContextCountsCanceledWrongPasswordFailure(t *testing.T) {
	s, d := newService(t)
	const (
		attempts = 5
		ip       = "198.51.100.23"
	)

	entered := make(chan struct{}, 1)
	var release chan struct{}
	s.verifyPassword = func(encoded, pw string) bool {
		entered <- struct{}{}
		<-release
		return false
	}
	t.Cleanup(func() { s.verifyPassword = VerifyPassword })

	for i := 0; i < attempts; i++ {
		release = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			token string
			err   error
		}
		done := make(chan result, 1)
		go func() {
			tok, err := s.LoginContext(ctx, DefaultUsername, "wrong-password", ip, "test")
			done <- result{token: tok, err: err}
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("attempt %d never reached the password check", i+1)
		}
		cancel()
		close(release)
		select {
		case r := <-done:
			if !errors.Is(r.err, context.Canceled) {
				t.Fatalf("canceled attempt %d err = %v, want context.Canceled", i+1, r.err)
			}
			if r.token != "" {
				t.Fatalf("canceled attempt %d issued token %q", i+1, r.token)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("canceled attempt %d did not return", i+1)
		}
	}

	s.limiter.mu.Lock()
	recorded := 0
	if a := s.limiter.m[ip]; a != nil {
		recorded = a.count
	}
	s.limiter.mu.Unlock()
	if recorded != attempts {
		t.Fatalf("limiter recorded %d canceled wrong-password failures, want %d", recorded, attempts)
	}
	if !s.limiter.Locked(ip) {
		t.Fatal("five canceled wrong-password attempts did not lock the IP")
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("sessions table has %d rows after canceled logins, want 0", rows)
	}
}

// TestLoginContextCorrectPasswordCanceledDoesNotCountFailure pins the other half of the accounting:
// a correct password whose client disconnects during the check returns its context error, records
// no failure and mints no session.
func TestLoginContextCorrectPasswordCanceledDoesNotCountFailure(t *testing.T) {
	s, d := newService(t)
	const ip = "198.51.100.24"

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	s.verifyPassword = func(encoded, pw string) bool {
		entered <- struct{}{}
		<-release
		return true
	}
	t.Cleanup(func() { s.verifyPassword = VerifyPassword })

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := s.LoginContext(ctx, DefaultUsername, DefaultPassword, ip, "test")
		done <- result{token: tok, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("login never reached the password check")
	}
	cancel()
	close(release)
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("LoginContext err = %v, want context.Canceled", r.err)
		}
		if r.token != "" {
			t.Fatalf("LoginContext issued token %q after its context was canceled", r.token)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoginContext did not return after its context was canceled")
	}

	s.limiter.mu.Lock()
	failures := 0
	if a := s.limiter.m[ip]; a != nil {
		failures = a.count
	}
	s.limiter.mu.Unlock()
	if failures != 0 || s.limiter.Locked(ip) {
		t.Fatalf("a canceled correct-password login recorded %d failures and locked=%v, want 0 and false",
			failures, s.limiter.Locked(ip))
	}
	var rows int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("sessions table has %d rows after a canceled correct-password login, want 0", rows)
	}
}
