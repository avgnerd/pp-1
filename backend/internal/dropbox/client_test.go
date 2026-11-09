package dropbox_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox"
	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
)

func setup(t *testing.T) (*fake.Server, *dropbox.Client) {
	t.Helper()
	fs, ts := fake.NewServer()
	t.Cleanup(ts.Close)
	ep := dropbox.Endpoints{API: ts.URL, Content: ts.URL, Authorize: ts.URL + "/oauth2/authorize", Token: ts.URL + "/oauth2/token"}
	tok, err := dropbox.ExchangeCode(context.Background(), nil, ep, "app1", "secret", "code-app1", "http://localhost:8080/api/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken == "" {
		t.Fatal("no refresh token")
	}
	return fs, dropbox.New(ep, "app1", "secret", tok.RefreshToken)
}

func TestHeaderSafeJSON(t *testing.T) {
	got, err := dropbox.HeaderSafeJSON(map[string]string{"path": "/한글 ü\x7f😀"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"path":"/\ud55c\uae00 \u00fc\u007f\ud83d\ude00"}`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	for _, r := range got {
		if r >= 0x7f {
			t.Fatalf("non-ASCII rune %q in header", r)
		}
	}
}

func TestListFolderPaging(t *testing.T) {
	fs, c := setup(t)
	fs.PageSize = 7
	a := fs.Account("app1")
	for i := 0; i < 50; i++ {
		a.Put(fmt.Sprintf("/docs/f%02d.txt", i), []byte("x"), time.Now())
	}
	var n int
	err := c.ListFolder(context.Background(), "/docs", false, func(es []dropbox.Metadata) error {
		n += len(es)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 50 {
		t.Fatalf("got %d entries", n)
	}
	if fs.CallCount("files/list_folder/continue") < 7 {
		t.Fatalf("expected continue calls, got %d", fs.CallCount("files/list_folder/continue"))
	}
}

func TestRetryOn429(t *testing.T) {
	fs, c := setup(t)
	fs.Account("app1").Mkdir("/a")
	fs.RateLimitEvery = 2 // second call is rate limited
	for i := 0; i < 4; i++ {
		if _, err := c.GetMetadata(context.Background(), "/a"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestRefreshOn401(t *testing.T) {
	fs, c := setup(t)
	fs.Account("app1").Mkdir("/a")
	if _, err := c.GetMetadata(context.Background(), "/a"); err != nil {
		t.Fatal(err)
	}
	before := fs.CallCount("oauth2/token")
	fs.ExpireTokens.Store(true)
	if _, err := c.GetMetadata(context.Background(), "/a"); err != nil {
		t.Fatal(err)
	}
	if fs.CallCount("oauth2/token") != before+1 {
		t.Fatal("expected exactly one refresh")
	}
}

func TestConcurrentRefreshSingleflight(t *testing.T) {
	fs, c := setup(t)
	fs.Account("app1").Mkdir("/a")
	before := fs.CallCount("oauth2/token")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetMetadata(context.Background(), "/a"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := fs.CallCount("oauth2/token") - before; got != 1 {
		t.Fatalf("refresh called %d times", got)
	}
}

func TestReauthCallback(t *testing.T) {
	fs, ts := fake.NewServer()
	defer ts.Close()
	_ = fs
	ep := dropbox.Endpoints{API: ts.URL, Content: ts.URL, Token: ts.URL + "/oauth2/token"}
	c := dropbox.New(ep, "app1", "secret", "bogus")
	called := false
	c.OnReauthNeeded = func() { called = true }
	_, err := c.GetMetadata(context.Background(), "/x")
	if err != dropbox.ErrReauth || !called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestBatchMove(t *testing.T) {
	fs, c := setup(t)
	a := fs.Account("app1")
	a.Put("/a/1.txt", []byte("1"), time.Now())
	a.Put("/a/2.txt", []byte("2"), time.Now())
	a.Put("/b/2.txt", []byte("x"), time.Now())
	fails, err := c.MoveBatch(context.Background(), []dropbox.RelocationEntry{
		{FromPath: "/a/1.txt", ToPath: "/b/1.txt"},
		{FromPath: "/a/2.txt", ToPath: "/b/2.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 1 || fails[0].Index != 1 || !strings.HasPrefix(fails[0].Summary, "to/conflict") {
		t.Fatalf("fails = %+v", fails)
	}
}

func TestThumbnailLimit(t *testing.T) {
	_, c := setup(t)
	paths := make([]string, 26)
	if _, err := c.GetThumbnailBatch(context.Background(), paths, "w256h256"); err == nil {
		t.Fatal("expected limit error")
	}
}

func TestAuthorizeURL(t *testing.T) {
	u := dropbox.AuthorizeURL(dropbox.Endpoints{Authorize: "https://www.dropbox.com/oauth2/authorize"}, "k", "", "st")
	if !strings.Contains(u, "token_access_type=offline") || strings.Contains(u, "redirect_uri") {
		t.Fatal(u)
	}
}
