package auth

import (
	"encoding/base64"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func policyPHC(memoryKiB, passes, threads, keyLen int) string {
	salt := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, keyLen))
	return "$argon2id$v=19$m=" + strconv.Itoa(memoryKiB) + ",t=" + strconv.Itoa(passes) +
		",p=" + strconv.Itoa(threads) + "$" + salt + "$" + key
}

// TestArgonPolicyPreserves256MiBCompatibility locks the restored-hash contract: earlier builds
// accepted 256 MiB, time 10 and 16 threads, so this build must keep accepting them. The burst is
// bounded by the single KDF slot instead of by lowering the ceiling. The default hash stays at the
// existing low-memory 64 MiB / 3 passes / 2 lanes so existing credentials keep their login cost.
func TestArgonPolicyPreserves256MiBCompatibility(t *testing.T) {
	if maxArgonMemoryKiB != 256*1024 {
		t.Fatalf("maxArgonMemoryKiB = %d, want the historical 256 MiB ceiling", maxArgonMemoryKiB)
	}
	if maxArgonTime < 10 || maxArgonThreads < 16 {
		t.Fatalf("policy bounds time=%d threads=%d, want to keep 10 and 16", maxArgonTime, maxArgonThreads)
	}
	if argonMemory != 64*1024 || argonTime != 3 || argonThreads != 2 {
		t.Fatalf("default argon2 parameters changed: m=%d t=%d p=%d", argonMemory, argonTime, argonThreads)
	}
	if kdfConcurrency != 1 {
		t.Fatalf("kdfConcurrency = %d, want one process-wide slot above the 256 MiB ceiling", kdfConcurrency)
	}
	if err := ValidatePasswordHash(policyPHC(256*1024, 10, 16, 32)); err != nil {
		t.Fatalf("a hash this product previously accepted is now rejected: %v", err)
	}
	if err := ValidatePasswordHash(policyPHC(64*1024, 3, 2, 32)); err != nil {
		t.Fatalf("the build's own hash policy was rejected: %v", err)
	}
	if err := ValidatePasswordHash(policyPHC(256*1024+1, 3, 2, 32)); err == nil {
		t.Fatal("a hash above the 256 MiB ceiling passed validation")
	}
}

// TestKDFAdmissionIsOneProcessWideSlot proves the admission policy directly: at no instant do two
// KDF bodies run together. With one slot and the 256 MiB ceiling this is the theoretical bound, not
// a heap-peak measurement that a lucky sampling window could miss.
func TestKDFAdmissionIsOneProcessWideSlot(t *testing.T) {
	const workers = 8
	var active, peak int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			runKDF(func() {
				n := atomic.AddInt32(&active, 1)
				for {
					seen := atomic.LoadInt32(&peak)
					if n <= seen || atomic.CompareAndSwapInt32(&peak, seen, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				atomic.AddInt32(&active, -1)
			})
		}()
	}
	close(start)
	wg.Wait()
	if peak != 1 {
		t.Fatalf("peak concurrent KDFs = %d, want 1 (one process-wide slot)", peak)
	}
}

// TestRejectedHashDoesNotTakeKDFSlot keeps policy rejection ahead of admission: a hostile or
// damaged hash must return without waiting behind a running KDF, or a login could hang for as long
// as an unrelated Argon2 attempt takes.
func TestRejectedHashDoesNotTakeKDFSlot(t *testing.T) {
	held := make(chan struct{})
	release := make(chan struct{})
	go runKDF(func() {
		close(held)
		<-release
	})
	<-held
	defer close(release)

	done := make(chan bool, 1)
	go func() { done <- VerifyPassword(policyPHC(512*1024, 3, 2, 32), "x") }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("an over-limit hash verified")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an over-limit hash blocked on the KDF slot instead of being rejected")
	}
}
