package fake_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
)

func getAuthorize(t *testing.T, clientID string) string {
	t.Helper()
	s := fake.New()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	u := ts.URL + "/oauth2/authorize?" + url.Values{"client_id": {clientID}}.Encode()
	resp, err := ts.Client().Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAuthorizePageEscapesClientID(t *testing.T) {
	body := getAuthorize(t, `<script>alert(1)</script>`)
	if strings.Contains(body, "<script>") {
		t.Fatalf("authorize page contains unescaped markup: %q", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("authorize page does not contain escaped markup: %q", body)
	}
}

func TestAuthorizePageKeepsPlainCode(t *testing.T) {
	body := getAuthorize(t, "app1")
	want := "<html><body>Access code: <code id=code>code-app1</code></body></html>"
	if body != want {
		t.Fatalf("authorize page = %q, want %q", body, want)
	}
}
