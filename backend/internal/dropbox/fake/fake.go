// Package fake is an in-memory Dropbox API server for tests and local demos.
//
// Each app key is one account. Any authorization code is accepted; the authorize page
// redirects straight back with a code. Files are held in memory.
package fake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Node is a file or folder.
type Node struct {
	Name     string
	Dir      bool
	Data     []byte
	Modified time.Time
	ID       string
	Rev      string
}

// Account is one fake Dropbox account.
type Account struct {
	Email string
	mu    sync.Mutex
	nodes map[string]*Node // key: lower-case path ("/a/b"); root is ""
}

// Server is the fake. Use NewServer for an httptest server or Handler for embedding.
type Server struct {
	mu       sync.Mutex
	accounts map[string]*Account // by app key
	tokens   map[string]string   // access token -> app key
	refresh  map[string]string   // refresh token -> app key
	seq      atomic.Int64
	BaseURL  string

	// Hooks for tests.
	Calls          sync.Map // endpoint -> *atomic.Int64
	RateLimitEvery int64    // if > 0, every Nth RPC call returns 429
	rpcCount       atomic.Int64
	ExpireTokens   atomic.Bool // when true the next request with any token gets 401 once
	PageSize       int         // list_folder page size (default 2000)
	Latency        time.Duration
	FailAccounts   sync.Map // app key -> true: every RPC returns 500
}

// New creates an empty fake.
func New() *Server {
	return &Server{accounts: map[string]*Account{}, tokens: map[string]string{}, refresh: map[string]string{}}
}

// NewServer starts an httptest server.
func NewServer() (*Server, *httptest.Server) {
	s := New()
	ts := httptest.NewServer(s.Handler())
	s.BaseURL = ts.URL
	return s, ts
}

// Account returns (creating) the account for an app key.
func (s *Server) Account(appKey string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[appKey]
	if !ok {
		a = &Account{Email: appKey + "@example.com", nodes: map[string]*Node{"": {Name: "", Dir: true}}}
		s.accounts[appKey] = a
	}
	return a
}

// Put adds a file (creating parent folders).
func (a *Account) Put(p string, data []byte, mod time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mkdirs(path.Dir(p))
	key := strings.ToLower(p)
	a.nodes[key] = &Node{Name: path.Base(p), Data: data, Modified: mod, ID: "id:" + key, Rev: fmt.Sprintf("%x", mod.UnixNano())}
}

// Mkdir creates a folder and its parents.
func (a *Account) Mkdir(p string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mkdirs(p)
}

func (a *Account) mkdirs(p string) {
	if p == "/" || p == "" || p == "." {
		return
	}
	a.mkdirs(path.Dir(p))
	key := strings.ToLower(p)
	if _, ok := a.nodes[key]; !ok {
		a.nodes[key] = &Node{Name: path.Base(p), Dir: true, ID: "id:" + key}
	}
}

func displayPath(a *Account, key string) string {
	if key == "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(key, "/"), "/")
	out := ""
	cur := ""
	for _, p := range parts {
		cur += "/" + p
		n := a.nodes[cur]
		name := p
		if n != nil {
			name = n.Name
		}
		out += "/" + name
	}
	return out
}

func (a *Account) meta(key string) map[string]any {
	n := a.nodes[key]
	m := map[string]any{"name": n.Name, "id": n.ID, "path_lower": key, "path_display": displayPath(a, key)}
	if n.Dir {
		m[".tag"] = "folder"
	} else {
		m[".tag"] = "file"
		m["size"] = len(n.Data)
		m["server_modified"] = n.Modified.UTC().Format(time.RFC3339)
		m["client_modified"] = n.Modified.UTC().Format(time.RFC3339)
		m["rev"] = n.Rev
		m["content_hash"] = "fake"
	}
	return m
}

func (a *Account) children(key string, recursive bool) []string {
	var out []string
	prefix := key + "/"
	for k := range a.nodes {
		if k == "" || !strings.HasPrefix(k, prefix) {
			continue
		}
		if !recursive && strings.Contains(k[len(prefix):], "/") {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) count(ep string) {
	v, _ := s.Calls.LoadOrStore(ep, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// CallCount returns how many times an endpoint was called.
func (s *Server) CallCount(ep string) int64 {
	v, ok := s.Calls.Load(ep)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

func writeErr(w http.ResponseWriter, status int, summary string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	tag := strings.SplitN(summary, "/", 2)[0]
	json.NewEncoder(w).Encode(map[string]any{"error_summary": summary + "/..", "error": map[string]any{".tag": tag}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Handler serves the fake API on one host (API, content and authorize share it).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/authorize", s.authorize)
	mux.HandleFunc("/oauth2/token", s.token)
	mux.HandleFunc("/tl/", s.tempLinkContent)
	mux.HandleFunc("/2/", s.api)
	return mux
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := "code-" + q.Get("client_id")
	if ru := q.Get("redirect_uri"); ru != "" {
		u, _ := url.Parse(ru)
		v := u.Query()
		v.Set("code", code)
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}
	fmt.Fprintf(w, "<html><body>Access code: <code id=code>%s</code></body></html>", html.EscapeString(code))
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	s.count("oauth2/token")
	r.ParseForm()
	appKey := r.Form.Get("client_id")
	if appKey == "" || r.Form.Get("client_secret") == "" {
		w.WriteHeader(400)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		if r.Form.Get("code") != "code-"+appKey {
			w.WriteHeader(400)
			writeJSON(w, map[string]string{"error": "invalid_grant", "error_description": "code doesn't exist or has expired"})
			return
		}
		rt := "rt-" + appKey
		s.refresh[rt] = appKey
		at := fmt.Sprintf("at-%s-%d", appKey, s.seq.Add(1))
		s.tokens[at] = appKey
		writeJSON(w, map[string]any{"access_token": at, "expires_in": 14400, "token_type": "bearer",
			"refresh_token": rt, "account_id": "dbid:" + appKey, "uid": "1", "scope": r.Form.Get("scope")})
	case "refresh_token":
		k, ok := s.refresh[r.Form.Get("refresh_token")]
		if !ok || k != appKey {
			w.WriteHeader(400)
			writeJSON(w, map[string]string{"error": "invalid_grant", "error_description": "refresh token is invalid or revoked"})
			return
		}
		at := fmt.Sprintf("at-%s-%d", appKey, s.seq.Add(1))
		s.tokens[at] = appKey
		writeJSON(w, map[string]any{"access_token": at, "expires_in": 14400, "token_type": "bearer"})
	default:
		w.WriteHeader(400)
		writeJSON(w, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (s *Server) accountFor(r *http.Request) (string, *Account, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	k, ok := s.tokens[tok]
	s.mu.Unlock()
	if !ok {
		return "", nil, false
	}
	return k, s.Account(k), true
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	ep := strings.TrimPrefix(r.URL.Path, "/2/")
	s.count(ep)
	if s.Latency > 0 {
		select {
		case <-time.After(s.Latency):
		case <-r.Context().Done():
			return
		}
	}
	if s.ExpireTokens.CompareAndSwap(true, false) {
		writeErr(w, 401, "expired_access_token")
		return
	}
	appKey, acct, ok := s.accountFor(r)
	if !ok {
		writeErr(w, 401, "invalid_access_token")
		return
	}
	if v, ok := s.FailAccounts.Load(appKey); ok && v.(bool) {
		writeErr(w, 500, "internal_error")
		return
	}
	if n := s.RateLimitEvery; n > 0 && s.rpcCount.Add(1)%n == 0 {
		w.Header().Set("Retry-After", "0")
		writeErr(w, 429, "too_many_requests")
		return
	}
	var arg map[string]any
	if h := r.Header.Get("Dropbox-API-Arg"); h != "" {
		json.Unmarshal([]byte(h), &arg)
	} else {
		b, _ := io.ReadAll(r.Body)
		if len(bytes.TrimSpace(b)) > 0 {
			json.Unmarshal(b, &arg)
		}
	}
	str := func(k string) string { v, _ := arg[k].(string); return v }
	acct.mu.Lock()
	defer acct.mu.Unlock()
	lookup := func(p string) (string, bool) {
		key := strings.ToLower(strings.TrimRight(p, "/"))
		if strings.HasPrefix(p, "id:") {
			for k, n := range acct.nodes {
				if n.ID == p {
					return k, true
				}
			}
			return "", false
		}
		_, ok := acct.nodes[key]
		return key, ok
	}
	switch ep {
	case "users/get_current_account":
		writeJSON(w, map[string]any{"account_id": "dbid:" + appKey, "email": acct.Email, "name": map[string]string{"display_name": "Fake " + appKey}})
	case "auth/token/revoke":
		s.mu.Lock()
		for t, k := range s.tokens {
			if k == appKey {
				delete(s.tokens, t)
			}
		}
		for t, k := range s.refresh {
			if k == appKey {
				delete(s.refresh, t)
			}
		}
		s.mu.Unlock()
		writeJSON(w, nil)
	case "files/list_folder", "files/list_folder/continue":
		var key string
		var recursive bool
		offset := 0
		if ep == "files/list_folder" {
			var ok bool
			key, ok = lookup(str("path"))
			if !ok || !acct.nodes[key].Dir {
				writeErr(w, 409, "path/not_found")
				return
			}
			recursive, _ = arg["recursive"].(bool)
		} else {
			c, _ := base64.RawURLEncoding.DecodeString(str("cursor"))
			parts := strings.SplitN(string(c), "|", 3)
			if len(parts) != 3 {
				writeErr(w, 409, "reset")
				return
			}
			offset, _ = strconv.Atoi(parts[0])
			recursive = parts[1] == "r"
			key = parts[2]
		}
		kids := acct.children(key, recursive)
		ps := s.PageSize
		if ps <= 0 {
			ps = 2000
		}
		end := offset + ps
		if end > len(kids) {
			end = len(kids)
		}
		entries := []map[string]any{}
		if recursive && offset == 0 && key != "" {
			entries = append(entries, acct.meta(key))
		}
		for _, k := range kids[offset:end] {
			entries = append(entries, acct.meta(k))
		}
		rflag := "n"
		if recursive {
			rflag = "r"
		}
		cursor := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d|%s|%s", end, rflag, key)))
		writeJSON(w, map[string]any{"entries": entries, "cursor": cursor, "has_more": end < len(kids)})
	case "files/get_metadata":
		key, ok := lookup(str("path"))
		if !ok || key == "" {
			writeErr(w, 409, "path/not_found")
			return
		}
		writeJSON(w, acct.meta(key))
	case "files/get_temporary_link":
		key, ok := lookup(str("path"))
		if !ok || acct.nodes[key].Dir {
			writeErr(w, 409, "path/not_found")
			return
		}
		link := s.BaseURL + "/tl/" + url.PathEscape(appKey) + "/" + base64.RawURLEncoding.EncodeToString([]byte(key)) + "/" + url.PathEscape(acct.nodes[key].Name)
		writeJSON(w, map[string]any{"metadata": acct.meta(key), "link": link})
	case "files/download", "files/get_preview":
		key, ok := lookup(str("path"))
		if !ok || acct.nodes[key].Dir {
			writeErr(w, 409, "path/not_found")
			return
		}
		n := acct.nodes[key]
		mb, _ := json.Marshal(acct.meta(key))
		w.Header().Set("Dropbox-API-Result", string(mb))
		if ep == "files/get_preview" {
			if strings.HasSuffix(key, ".xlsx") || strings.HasSuffix(key, ".csv") {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, "<table><tr><td>%s</td></tr></table>", n.Name)
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF-1.4 fake"))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, n.Name, n.Modified, bytes.NewReader(n.Data))
	case "files/get_thumbnail_batch":
		es, _ := arg["entries"].([]any)
		if len(es) > 25 {
			writeErr(w, 400, "too_many_files")
			return
		}
		out := []map[string]any{}
		for _, e := range es {
			em, _ := e.(map[string]any)
			p, _ := em["path"].(string)
			key, ok := lookup(p)
			if !ok {
				out = append(out, map[string]any{".tag": "failure", "failure": map[string]any{".tag": "path"}})
				continue
			}
			out = append(out, map[string]any{".tag": "success", "metadata": acct.meta(key),
				"thumbnail": base64.StdEncoding.EncodeToString(tinyJPEG)})
		}
		writeJSON(w, map[string]any{"entries": out})
	case "files/search_v2", "files/search/continue_v2":
		q := strings.ToLower(str("query"))
		scope := ""
		if opts, ok := arg["options"].(map[string]any); ok {
			if p, ok := opts["path"].(string); ok {
				scope = strings.ToLower(p)
			}
		}
		if ep == "files/search/continue_v2" {
			writeJSON(w, map[string]any{"matches": []any{}, "has_more": false})
			return
		}
		matches := []map[string]any{}
		keys := make([]string, 0, len(acct.nodes))
		for k := range acct.nodes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "" || (scope != "" && !strings.HasPrefix(k, scope+"/")) {
				continue
			}
			if strings.Contains(strings.ToLower(acct.nodes[k].Name), q) {
				matches = append(matches, map[string]any{"metadata": map[string]any{".tag": "metadata", "metadata": acct.meta(k)}})
			}
		}
		writeJSON(w, map[string]any{"matches": matches, "has_more": false})
	case "files/move_v2", "files/copy_v2":
		from, ok := lookup(str("from_path"))
		if !ok {
			writeErr(w, 409, "from_lookup/not_found")
			return
		}
		to := str("to_path")
		if _, exists := acct.nodes[strings.ToLower(to)]; exists {
			writeErr(w, 409, "to/conflict/file")
			return
		}
		acct.relocate(from, to, ep == "files/move_v2")
		writeJSON(w, map[string]any{"metadata": acct.meta(strings.ToLower(to))})
	case "files/delete_v2":
		key, ok := lookup(str("path"))
		if !ok || key == "" {
			writeErr(w, 409, "path_lookup/not_found")
			return
		}
		m := acct.meta(key)
		acct.remove(key)
		writeJSON(w, map[string]any{"metadata": m})
	case "files/move_batch_v2", "files/copy_batch_v2", "files/delete_batch":
		es, _ := arg["entries"].([]any)
		results := []map[string]any{}
		for _, e := range es {
			em, _ := e.(map[string]any)
			if ep == "files/delete_batch" {
				p, _ := em["path"].(string)
				key, ok := lookup(p)
				if !ok {
					results = append(results, map[string]any{".tag": "failure", "failure": map[string]any{".tag": "path_lookup", "path_lookup": map[string]any{".tag": "not_found"}}})
					continue
				}
				acct.remove(key)
				results = append(results, map[string]any{".tag": "success"})
				continue
			}
			fp, _ := em["from_path"].(string)
			tp, _ := em["to_path"].(string)
			from, ok := lookup(fp)
			if !ok {
				results = append(results, map[string]any{".tag": "failure", "failure": map[string]any{".tag": "from_lookup", "from_lookup": map[string]any{".tag": "not_found"}}})
				continue
			}
			if _, exists := acct.nodes[strings.ToLower(tp)]; exists {
				results = append(results, map[string]any{".tag": "failure", "failure": map[string]any{".tag": "to", "to": map[string]any{".tag": "conflict"}}})
				continue
			}
			acct.relocate(from, tp, ep == "files/move_batch_v2")
			results = append(results, map[string]any{".tag": "success"})
		}
		job := fmt.Sprintf("job-%d", s.seq.Add(1))
		jobResults.Store(job, results)
		writeJSON(w, map[string]any{".tag": "async_job_id", "async_job_id": job})
	case "files/move_batch/check_v2", "files/copy_batch/check_v2", "files/delete_batch/check":
		v, ok := jobResults.Load(str("async_job_id"))
		if !ok {
			writeErr(w, 409, "invalid_async_job_id")
			return
		}
		writeJSON(w, map[string]any{".tag": "complete", "entries": v})
	default:
		writeErr(w, 400, "unknown_endpoint")
	}
}

var jobResults sync.Map

func (a *Account) remove(key string) {
	for _, k := range a.children(key, true) {
		delete(a.nodes, k)
	}
	delete(a.nodes, key)
}

func (a *Account) relocate(from, to string, move bool) {
	toKey := strings.ToLower(to)
	a.mkdirs(path.Dir(to))
	src := a.nodes[from]
	cp := *src
	cp.Name = path.Base(to)
	// Dropbox keeps the stable id on a move and assigns a new one on a copy. The id is what a
	// retried move can be reconciled against, so the fake must model that distinction.
	if !move {
		cp.ID = "id:" + toKey
	}
	a.nodes[toKey] = &cp
	for _, k := range a.children(from, true) {
		n := *a.nodes[k]
		nk := toKey + k[len(from):]
		if !move {
			n.ID = "id:" + nk
		}
		a.nodes[nk] = &n
	}
	if move {
		a.remove(from)
	}
}

func (s *Server) tempLinkContent(w http.ResponseWriter, r *http.Request) {
	s.count("templink")
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/tl/"), "/", 3)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	appKey, _ := url.PathUnescape(parts[0])
	keyB, _ := base64.RawURLEncoding.DecodeString(parts[1])
	a := s.Account(appKey)
	a.mu.Lock()
	n, ok := a.nodes[string(keyB)]
	a.mu.Unlock()
	if !ok || n.Dir {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, n.Name, n.Modified, bytes.NewReader(n.Data))
}

// tinyJPEG is a 1x1 JPEG used as every thumbnail.
var tinyJPEG, _ = base64.StdEncoding.DecodeString(
	"/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/" +
		"wAALCAABAAEBAREA/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAD8AKp//2Q==")
