package dropbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
)

// newScriptedClient serves the token endpoint and the Dropbox RPC host from one test server, so a
// test can script a batch launch, its read-only polling checks, and token-refresh failures.
func newScriptedClient(t *testing.T, token, api http.HandlerFunc) *dropbox.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			token(w, r)
			return
		}
		api(w, r)
	}))
	t.Cleanup(srv.Close)
	return dropbox.New(
		dropbox.Endpoints{API: srv.URL, Content: srv.URL, Token: srv.URL + "/oauth2/token"},
		"app1", "secret", "refresh-1",
	)
}

func writeTestJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func writeTestError(w http.ResponseWriter, status int, summary string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error_summary": summary + "/.."})
}

func tokenAlwaysOK(w http.ResponseWriter, _ *http.Request) {
	writeTestJSON(w, map[string]any{"access_token": "at-1", "expires_in": 14400, "token_type": "bearer"})
}

func batchLaunch(w http.ResponseWriter, _ *http.Request) {
	writeTestJSON(w, map[string]any{".tag": "async_job_id", "async_job_id": "job-1"})
}

func completeWithConflict(w http.ResponseWriter) {
	writeTestJSON(w, map[string]any{
		".tag": "complete",
		"entries": []any{
			map[string]any{".tag": "success"},
			map[string]any{".tag": "failure", "failure": map[string]any{
				".tag": "to", "to": map[string]any{".tag": "conflict"},
			}},
		},
	})
}

// TestBatchCheckTransientRetryDoesNotMarkLaunchRetried locks down the meaning of Retried: a
// read-only check that retried its own 503 did not apply the launch twice. The completed conflict
// is therefore still definitive.
func TestBatchCheckTransientRetryDoesNotMarkLaunchRetried(t *testing.T) {
	var checks atomic.Int64
	api := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/files/move_batch_v2":
			batchLaunch(w, r)
		case "/2/files/move_batch/check_v2":
			if checks.Add(1) == 1 {
				writeTestError(w, http.StatusServiceUnavailable, "internal_error")
				return
			}
			completeWithConflict(w)
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	res, err := c.MoveBatchOutcome(context.Background(), []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if err != nil {
		t.Fatalf("MoveBatchOutcome returned error: %v", err)
	}
	if res.Retried {
		t.Fatalf("a read-only check retry marked the launch Retried; failures = %+v", res.Failures)
	}
	if len(res.Failures) != 1 || res.Failures[0].Index != 1 || res.Failures[0].Summary != "to/conflict" {
		t.Fatalf("failures = %+v, want one to/conflict at index 1", res.Failures)
	}
	if dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("a completed, non-retried conflict was reported as an unknown write: %v", err)
	}
}

// TestBatchPollingRateLimitAfterLaunchIsAmbiguous models a check the client refuses to wait out.
// The launch started the job, so failing to observe it to completion must not become a confirmed
// failure even though the surfacing error is a definitive 429.
func TestBatchPollingRateLimitAfterLaunchIsAmbiguous(t *testing.T) {
	api := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/files/move_batch_v2":
			batchLaunch(w, r)
		case "/2/files/move_batch/check_v2":
			w.Header().Set("Retry-After", "61")
			writeTestError(w, http.StatusTooManyRequests, "too_many_requests")
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	res, err := c.MoveBatchOutcome(context.Background(), []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if err == nil {
		t.Fatal("MoveBatchOutcome succeeded, want a polling failure")
	}
	if !res.Retried {
		t.Fatalf("polling failure after async_job_id: Retried = false, error = %v", err)
	}
	if !dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("polling failure after async_job_id: WriteOutcomeUnknown = false, error = %v", err)
	}
}

// TestBatchPollingUnexpectedTagAfterLaunchIsAmbiguous covers a job that ends in a tag the client
// does not recognize. The job existed, so its write outcome is unknown rather than failed.
func TestBatchPollingUnexpectedTagAfterLaunchIsAmbiguous(t *testing.T) {
	api := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/files/move_batch_v2":
			batchLaunch(w, r)
		case "/2/files/move_batch/check_v2":
			writeTestJSON(w, map[string]any{".tag": "failed"})
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	res, err := c.MoveBatchOutcome(context.Background(), []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if err == nil {
		t.Fatal("MoveBatchOutcome succeeded, want an unexpected-tag error")
	}
	if !res.Retried || !dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("Retried = %v, WriteOutcomeUnknown = %v after an unknown job tag; want both true (err: %v)",
			res.Retried, dropbox.WriteOutcomeUnknown(err), err)
	}
}

// TestBatchPollingCancellationAfterLaunchIsAmbiguous cancels the caller while the client waits to
// poll. The batch was already accepted, so the cancellation must not read as a confirmed failure.
func TestBatchPollingCancellationAfterLaunchIsAmbiguous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/files/move_batch_v2" {
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		batchLaunch(w, r)
		// The launch response is complete; cancel during the first 300ms polling wait.
		time.AfterFunc(150*time.Millisecond, cancel)
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	res, err := c.MoveBatchOutcome(ctx, []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the polling cancellation", err)
	}
	if !res.Retried || !dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("Retried = %v, WriteOutcomeUnknown = %v after cancellation; want both true (err: %v)",
			res.Retried, dropbox.WriteOutcomeUnknown(err), err)
	}
}

// TestBatchCheckRateLimitThenCompleteStaysDefinitive proves the other direction: a check that
// retries a 429 and then completes with a conflict did not retry the write, so the conflict stays
// a definitive failure.
func TestBatchCheckRateLimitThenCompleteStaysDefinitive(t *testing.T) {
	var checks atomic.Int64
	api := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/files/move_batch_v2":
			batchLaunch(w, r)
		case "/2/files/move_batch/check_v2":
			if checks.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				writeTestError(w, http.StatusTooManyRequests, "too_many_requests")
				return
			}
			completeWithConflict(w)
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	res, err := c.MoveBatchOutcome(context.Background(), []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if err != nil {
		t.Fatalf("MoveBatchOutcome returned error: %v", err)
	}
	if res.Retried {
		t.Fatalf("a check 429 retry marked the launch Retried; failures = %+v", res.Failures)
	}
	if len(res.Failures) != 1 || res.Failures[0].Summary != "to/conflict" {
		t.Fatalf("failures = %+v, want one to/conflict", res.Failures)
	}
	if dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("a completed, non-retried conflict was reported as an unknown write: %v", err)
	}
}

// TestBatchCheckRefreshFailureAfterLaunchIsAmbiguous: the launch succeeded, then polling got a 401
// whose refresh failed. That leaves the job outcome unknown.
func TestBatchCheckRefreshFailureAfterLaunchIsAmbiguous(t *testing.T) {
	var tokenCalls atomic.Int64
	token := func(w http.ResponseWriter, r *http.Request) {
		if tokenCalls.Add(1) == 1 {
			tokenAlwaysOK(w, r)
			return
		}
		writeTestError(w, http.StatusInternalServerError, "server_error")
	}
	api := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/files/move_batch_v2":
			batchLaunch(w, r)
		case "/2/files/move_batch/check_v2":
			writeTestError(w, http.StatusUnauthorized, "invalid_access_token")
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	c := newScriptedClient(t, token, api)

	res, err := c.MoveBatchOutcome(context.Background(), []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if err == nil {
		t.Fatal("MoveBatchOutcome succeeded, want a refresh failure")
	}
	if !res.Retried || !dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("Retried = %v, WriteOutcomeUnknown = %v after a polling 401 refresh failure; want both true (err: %v)",
			res.Retried, dropbox.WriteOutcomeUnknown(err), err)
	}
}

// TestDeleteBatchPropagatesPollingAmbiguity checks the error-only DeleteBatch path keeps the same
// ambiguity a failed poll carries for move and copy batches.
func TestDeleteBatchPropagatesPollingAmbiguity(t *testing.T) {
	api := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2/files/delete_batch":
			batchLaunch(w, r)
		case "/2/files/delete_batch/check":
			w.Header().Set("Retry-After", "61")
			writeTestError(w, http.StatusTooManyRequests, "too_many_requests")
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	if _, err := c.DeleteBatch(context.Background(), []string{"/a"}); err == nil {
		t.Fatal("DeleteBatch succeeded, want a polling failure")
	} else if !dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("DeleteBatch polling failure: WriteOutcomeUnknown = false, error = %v", err)
	}
}

// TestBatchLaunchErrorAfterAmbiguousAttemptIsAmbiguous covers a launch whose first attempt got a
// 503 (so the batch may already have applied) and whose second attempt got a 200 with a body that
// does not decode. The launch error must carry the earlier attempt's ambiguity instead of reading
// as a confirmed failure, which would let a retry duplicate the moves.
func TestBatchLaunchErrorAfterAmbiguousAttemptIsAmbiguous(t *testing.T) {
	var launches atomic.Int64
	api := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/files/move_batch_v2" {
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if launches.Add(1) == 1 {
			writeTestError(w, http.StatusServiceUnavailable, "internal_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"async_job_id":`)) // truncated JSON body after a 200
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	res, err := c.MoveBatchOutcome(context.Background(), []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if err == nil {
		t.Fatal("MoveBatchOutcome succeeded, want the truncated launch body error")
	}
	if !res.Retried {
		t.Fatalf("truncated launch body after a 503: Retried = false, error = %v", err)
	}
	if !dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("truncated launch body after a 503: WriteOutcomeUnknown = false, error = %v", err)
	}
}

// TestBatchUnexpectedLaunchTagAfterAmbiguousAttemptIsAmbiguous covers the other launch path that
// returns no async_job_id: a 503 was already answered, so the tagless 200 may describe an applied
// batch and its error must stay ambiguous too.
func TestBatchUnexpectedLaunchTagAfterAmbiguousAttemptIsAmbiguous(t *testing.T) {
	var launches atomic.Int64
	api := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/files/move_batch_v2" {
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if launches.Add(1) == 1 {
			writeTestError(w, http.StatusServiceUnavailable, "internal_error")
			return
		}
		writeTestJSON(w, map[string]any{".tag": "unexpected"})
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	res, err := c.MoveBatchOutcome(context.Background(), []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if err == nil {
		t.Fatal("MoveBatchOutcome succeeded, want an unexpected launch tag error")
	}
	if !res.Retried || !dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("Retried = %v, WriteOutcomeUnknown = %v after an unexpected launch tag; want both true (err: %v)",
			res.Retried, dropbox.WriteOutcomeUnknown(err), err)
	}
}

// TestBatchFreshLaunchErrorIsDefinitive is the paired negative: a launch that fails on its first
// attempt never reached a state where the batch might have applied.
func TestBatchFreshLaunchErrorIsDefinitive(t *testing.T) {
	api := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/files/move_batch_v2" {
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeTestError(w, http.StatusBadRequest, "bad_request")
	}
	c := newScriptedClient(t, tokenAlwaysOK, api)

	res, err := c.MoveBatchOutcome(context.Background(), []dropbox.RelocationEntry{{FromPath: "/a", ToPath: "/b"}})
	if err == nil {
		t.Fatal("MoveBatchOutcome succeeded, want a 400")
	}
	if res.Retried {
		t.Fatalf("a fresh launch 400 marked the batch Retried, error = %v", err)
	}
	if dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("a fresh launch 400 was reported as an unknown write: %v", err)
	}
}

// TestRefreshFailureAfterAmbiguousAttemptIsAmbiguous covers the transport layer directly: a first
// attempt got a 503 (so the request may have applied), then a 401's refresh failed.
func TestRefreshFailureAfterAmbiguousAttemptIsAmbiguous(t *testing.T) {
	var tokenCalls atomic.Int64
	token := func(w http.ResponseWriter, r *http.Request) {
		if tokenCalls.Add(1) == 1 {
			tokenAlwaysOK(w, r)
			return
		}
		writeTestError(w, http.StatusInternalServerError, "server_error")
	}
	var apiCalls atomic.Int64
	api := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/files/get_metadata" {
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
		if apiCalls.Add(1) == 1 {
			writeTestError(w, http.StatusServiceUnavailable, "internal_error")
			return
		}
		writeTestError(w, http.StatusUnauthorized, "invalid_access_token")
	}
	c := newScriptedClient(t, token, api)

	if _, err := c.GetMetadata(context.Background(), "/a"); err == nil {
		t.Fatal("GetMetadata succeeded, want a refresh failure")
	} else if !dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("refresh failure after an ambiguous attempt: WriteOutcomeUnknown = false, error = %v", err)
	}
}

// TestRefreshFailureOnFreshRequestIsDefinitive is the paired negative: a first 401 whose refresh
// failed never let the request reach a state where it might have applied.
func TestRefreshFailureOnFreshRequestIsDefinitive(t *testing.T) {
	var tokenCalls atomic.Int64
	token := func(w http.ResponseWriter, r *http.Request) {
		if tokenCalls.Add(1) == 1 {
			tokenAlwaysOK(w, r)
			return
		}
		writeTestError(w, http.StatusInternalServerError, "server_error")
	}
	api := func(w http.ResponseWriter, r *http.Request) {
		writeTestError(w, http.StatusUnauthorized, "invalid_access_token")
	}
	c := newScriptedClient(t, token, api)

	if _, err := c.GetMetadata(context.Background(), "/a"); err == nil {
		t.Fatal("GetMetadata succeeded, want a refresh failure")
	} else if dropbox.WriteOutcomeUnknown(err) {
		t.Fatalf("a fresh 401 whose refresh failed was reported as an unknown write: %v", err)
	}
}
