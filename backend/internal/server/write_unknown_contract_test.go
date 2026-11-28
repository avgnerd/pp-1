package server

import (
	"net/http"
	"testing"
	"time"
)

// A write whose outcome Dropbox could not confirm is neither success nor a confirmed failure.
// The client relies on all three parts of the response: 502, unknown true, a non-empty detail,
// and the Korean instruction to refresh before retrying.
func TestFailWriteUnknownContract(t *testing.T) {
	e, p := newWriteFaultEnv(t)
	e.fake.Account("app1").Put("/doc.txt", []byte("hello"), time.Now())
	// Populate the listing cache first: the fault is armed for every transport call, including
	// the stat that precedes the write.
	_ = writeList(t, e, "/m")
	p.arm("", writeFaultAllTransport)

	status, body := postWriteOp(t, e, "/api/fs/rename", map[string]string{"path": "/m/doc.txt", "name": "renamed.txt"})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %v)", status, body)
	}
	if unknown, _ := body["unknown"].(bool); !unknown {
		t.Fatalf("unknown = %v, want true (body %v)", body["unknown"], body)
	}
	if detail, _ := body["detail"].(string); detail == "" {
		t.Fatalf("detail = %v, want the unconfirmed write's error text (body %v)", body["detail"], body)
	}
	const wantError = "Dropbox 쓰기 결과를 확인할 수 없습니다. 목록을 새로고침한 뒤 다시 시도하세요"
	if got, _ := body["error"].(string); got != wantError {
		t.Fatalf("error = %q, want %q", got, wantError)
	}
}
