package server

import "testing"

// A storage row that no longer exists is a 404 on the mutation paths as well, not a 500 carrying an
// internal error. The admin page refreshes on 404 and treats 500 as a server fault.
func TestStorageMutationOnMissingRowIs404(t *testing.T) {
	e := newEnv(t)
	e.login()
	body := map[string]any{"name": "gone", "mount_path": "/gone", "root_path": "/", "cache_ttl_min": 30}
	if st := e.do("PUT", "/api/admin/storages/4242", body, nil); st != 404 {
		t.Fatalf("PUT missing storage: %d, want 404", st)
	}
	if st := e.do("POST", "/api/admin/storages/4242/reauth", map[string]any{"auth_mode": "localhost"}, nil); st != 404 {
		t.Fatalf("reauth missing storage: %d, want 404", st)
	}
	if st := e.do("DELETE", "/api/admin/storages/4242", nil, nil); st != 404 {
		t.Fatalf("DELETE missing storage: %d, want 404", st)
	}
}
