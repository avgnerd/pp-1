// Package dropbox is a small Dropbox HTTP API v2 client covering the endpoints this app uses.
//
// It separates RPC calls (api.dropboxapi.com, JSON body) from content calls
// (content.dropboxapi.com, arguments in the Dropbox-API-Arg header, escaped to ASCII),
// refreshes short-lived access tokens from a refresh token (singleflight per account),
// retries once after a 401, and honours Retry-After on 429.
package dropbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// Endpoints are the Dropbox base URLs; overridable for tests.
type Endpoints struct {
	API       string // https://api.dropboxapi.com
	Content   string // https://content.dropboxapi.com
	Authorize string // https://www.dropbox.com/oauth2/authorize
	Token     string // https://api.dropboxapi.com/oauth2/token
}

// Scopes requested at authorization time.
const Scopes = "account_info.read files.metadata.read files.content.read files.content.write"

// SharedTransport is tuned for many parallel calls across 100+ accounts and for streaming.
var SharedTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          512,
	MaxIdleConnsPerHost:   128,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
	ReadBufferSize:        256 << 10,
	WriteBufferSize:       64 << 10,
}

// HTTPClient has no overall timeout because content responses stream for a long time;
// per-request contexts bound RPC calls instead.
var HTTPClient = &http.Client{Transport: SharedTransport}

// APIError is a non-2xx Dropbox response.
type APIError struct {
	Status     int
	Summary    string
	RetryAfter time.Duration
	Body       string

	// Retried reports that an earlier attempt of this same request ended without a definitive
	// answer (a transport error or a 5xx), so this 409 may describe a write that earlier attempt
	// already applied. A non-idempotent caller reads it through WriteOutcomeUnknown.
	Retried bool
}

func (e *APIError) Error() string {
	if e.Summary != "" {
		return fmt.Sprintf("dropbox %d: %s", e.Status, e.Summary)
	}
	return fmt.Sprintf("dropbox %d: %s", e.Status, truncate(e.Body, 200))
}

// HasPrefix reports whether the error summary starts with prefix (Dropbox recommends prefix matching).
func (e *APIError) HasPrefix(prefix string) bool { return strings.HasPrefix(e.Summary, prefix) }

// HasSummary reports whether err is an APIError whose summary contains part.
func HasSummary(err error, part string) bool {
	var ae *APIError
	return errors.As(err, &ae) && strings.Contains(ae.Summary, part)
}

// ambiguousTransportError marks a request whose earlier attempt got no answer and whose final
// attempt also ended without one. The write may have reached Dropbox, so callers must reconcile it
// instead of reporting a confirmed failure. A transport error on a fresh request is not wrapped:
// that attempt never reached the server.
type ambiguousTransportError struct{ err error }

func (e *ambiguousTransportError) Error() string { return e.err.Error() }
func (e *ambiguousTransportError) Unwrap() error { return e.err }

// WriteOutcomeUnknown reports whether a failed non-idempotent write may have been applied. It is
// true for a retried request whose earlier attempt got no definitive answer, and for a request
// whose final transport attempt ended without one. A missing APIError alone is not enough: a local
// build error or a cancellation is not an ambiguous write.
func WriteOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Retried
	}
	var te *ambiguousTransportError
	return errors.As(err, &te)
}

// ambiguousFailure wraps err only when an earlier attempt was already ambiguous, so cancellation
// and sleep deadlines after an unanswered attempt still report an unknown write.
func ambiguousFailure(ambiguous bool, err error) error {
	if ambiguous {
		return &ambiguousTransportError{err: err}
	}
	return err
}

// retryMarker records, per request, whether an earlier attempt got no answer. A batch call returns
// per-entry results rather than one APIError, so it carries this marker to know whether a failure
// summary may describe an earlier attempt that was already applied.
type retryMarker struct{ ambiguous atomic.Bool }

type retryMarkerKey struct{}

func withRetryMarker(ctx context.Context) (context.Context, *retryMarker) {
	if m, ok := ctx.Value(retryMarkerKey{}).(*retryMarker); ok {
		return ctx, m
	}
	m := &retryMarker{}
	return context.WithValue(ctx, retryMarkerKey{}, m), m
}

func markRetried(ctx context.Context) {
	if m, ok := ctx.Value(retryMarkerKey{}).(*retryMarker); ok {
		m.ambiguous.Store(true)
	}
}

// ErrReauth means the refresh token was rejected; the account must be re-authorized.
var ErrReauth = errors.New("dropbox: 계정 인증이 만료되었거나 취소되었습니다. 관리자 저장소 메뉴에서 다시 인증하세요")

// Token is an /oauth2/token response.
type Token struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	UID          string `json:"uid"`
}

// Client talks to Dropbox on behalf of one account.
type Client struct {
	ep           Endpoints
	http         *http.Client
	appKey       string
	appSecret    string
	refreshToken string

	mu      sync.Mutex
	access  string
	expires time.Time
	sf      singleflight.Group

	// OnReauthNeeded is called once when the refresh token is rejected.
	OnReauthNeeded func()
	reauthOnce     sync.Once
}

// New creates a client for one account.
func New(ep Endpoints, appKey, appSecret, refreshToken string) *Client {
	return &Client{ep: ep, http: HTTPClient, appKey: appKey, appSecret: appSecret, refreshToken: refreshToken}
}

// SetHTTPClient replaces the HTTP client (tests).
func (c *Client) SetHTTPClient(h *http.Client) { c.http = h }

// AccessToken returns a valid access token, refreshing it when it is missing or
// within five minutes of expiry.
func (c *Client) AccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.access != "" && time.Until(c.expires) > 5*time.Minute {
		t := c.access
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	return c.refresh(ctx, "")
}

// refresh obtains a new access token. stale is the token that just failed (if any) so
// concurrent 401s trigger a single refresh.
func (c *Client) refresh(ctx context.Context, stale string) (string, error) {
	v, err, _ := c.sf.Do("refresh", func() (any, error) {
		c.mu.Lock()
		if c.access != "" && c.access != stale && time.Until(c.expires) > 5*time.Minute {
			t := c.access
			c.mu.Unlock()
			return t, nil
		}
		c.mu.Unlock()
		// Detach from the caller so one cancelled request does not fail every waiter.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		tok, err := RefreshAccessToken(rctx, c.http, c.ep, c.appKey, c.appSecret, c.refreshToken)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 401) &&
				(strings.Contains(ae.Body, "invalid_grant") || strings.Contains(ae.Body, "invalid_client")) {
				c.reauthOnce.Do(func() {
					if c.OnReauthNeeded != nil {
						c.OnReauthNeeded()
					}
				})
				return "", ErrReauth
			}
			return "", err
		}
		exp := time.Duration(tok.ExpiresIn) * time.Second
		if exp <= 0 {
			exp = 4 * time.Hour
		}
		c.mu.Lock()
		c.access = tok.AccessToken
		c.expires = time.Now().Add(exp)
		c.mu.Unlock()
		return tok.AccessToken, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// ExchangeCode trades an authorization code for tokens. redirectURI must equal the one
// used in the authorization URL ("" when the authorization URL had none).
func ExchangeCode(ctx context.Context, h *http.Client, ep Endpoints, appKey, appSecret, code, redirectURI string) (*Token, error) {
	form := url.Values{"code": {code}, "grant_type": {"authorization_code"}}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	return tokenRequest(ctx, h, ep, appKey, appSecret, form)
}

// RefreshAccessToken uses a refresh token to obtain a new short-lived access token.
func RefreshAccessToken(ctx context.Context, h *http.Client, ep Endpoints, appKey, appSecret, refreshToken string) (*Token, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
	return tokenRequest(ctx, h, ep, appKey, appSecret, form)
}

func tokenRequest(ctx context.Context, h *http.Client, ep Endpoints, appKey, appSecret string, form url.Values) (*Token, error) {
	if h == nil {
		h = HTTPClient
	}
	form.Set("client_id", appKey)
	form.Set("client_secret", appSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		ae := &APIError{Status: resp.StatusCode, Body: string(body)}
		var e struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			ae.Summary = e.Error
			if e.ErrorDescription != "" {
				ae.Summary += ": " + e.ErrorDescription
			}
		}
		return nil, ae
	}
	var t Token
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// AuthorizeURL builds the /oauth2/authorize URL with offline access.
func AuthorizeURL(ep Endpoints, appKey, redirectURI, state string) string {
	q := url.Values{
		"client_id":         {appKey},
		"response_type":     {"code"},
		"token_access_type": {"offline"},
		"scope":             {Scopes},
		"state":             {state},
	}
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	return ep.Authorize + "?" + q.Encode()
}

const maxRetries = 4

// RPC calls an RPC endpoint (e.g. "files/list_folder") with a JSON argument and decodes the result.
// arg == nil sends no body (for endpoints that take no arguments).
func (c *Client) RPC(ctx context.Context, endpoint string, arg, out any) error {
	return c.rpcAt(ctx, c.ep.API, endpoint, arg, out)
}

func (c *Client) rpcAt(ctx context.Context, host, endpoint string, arg, out any) error {
	var payload []byte
	if arg != nil {
		var err error
		if payload, err = json.Marshal(arg); err != nil {
			return err
		}
	}
	resp, err := c.do(ctx, func(token string) (*http.Request, error) {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/2/"+endpoint, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Content calls a content-download endpoint (e.g. "files/download") with arguments in the
// Dropbox-API-Arg header. The caller must close the response body. Extra headers such as
// Range are forwarded. 206 and 304 responses are returned without error.
func (c *Client) Content(ctx context.Context, endpoint string, arg any, headers http.Header) (*http.Response, error) {
	argJSON, err := HeaderSafeJSON(arg)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ep.Content+"/2/"+endpoint, nil)
		if err != nil {
			return nil, err
		}
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Dropbox-API-Arg", argJSON)
		return req, nil
	})
}

// do runs a request with auth refresh on 401 and backoff on 429/5xx.
func (c *Client) do(ctx context.Context, build func(token string) (*http.Request, error)) (*http.Response, error) {
	token, err := c.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	refreshed := false
	// ambiguous records that an earlier attempt got no answer, so a later 409 can describe a write
	// this request already applied. A 429 is not ambiguous: Dropbox answered without applying it.
	ambiguous := false
	for attempt := 0; ; attempt++ {
		req, err := build(token)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil || attempt >= 2 {
				return nil, ambiguousFailure(ambiguous || attempt >= 2, err)
			}
			ambiguous = true
			markRetried(ctx)
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, ambiguousFailure(ambiguous, err)
			}
			continue
		}
		if resp.StatusCode < 300 || resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusNotModified {
			return resp, nil
		}
		apiErr := readError(resp)
		apiErr.Retried = ambiguous
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !refreshed && !apiErr.HasPrefix("missing_scope"):
			refreshed = true
			if token, err = c.refresh(ctx, token); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries:
			wait := apiErr.RetryAfter
			if wait <= 0 {
				wait = backoff(attempt)
			}
			if wait > 60*time.Second {
				return nil, apiErr
			}
			if err := sleep(ctx, wait); err != nil {
				return nil, ambiguousFailure(ambiguous, err)
			}
			continue
		case (resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 503) && attempt < 2:
			ambiguous = true
			markRetried(ctx)
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, ambiguousFailure(ambiguous, err)
			}
			continue
		}
		return nil, apiErr
	}
}

func readError(resp *http.Response) *APIError {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ae := &APIError{Status: resp.StatusCode, Body: string(body)}
	var e struct {
		ErrorSummary string `json:"error_summary"`
		Error        struct {
			RetryAfter int `json:"retry_after"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		ae.Summary = e.ErrorSummary
		if e.Error.RetryAfter > 0 {
			ae.RetryAfter = time.Duration(e.Error.RetryAfter) * time.Second
		}
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil && n >= 0 {
			ae.RetryAfter = time.Duration(n) * time.Second
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests && ae.RetryAfter == 0 {
		ae.RetryAfter = time.Second
	}
	return ae
}

func backoff(attempt int) time.Duration {
	base := time.Duration(1<<attempt) * 500 * time.Millisecond
	return base + time.Duration(rand.Int64N(int64(base/2)+1))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// HeaderSafeJSON marshals v and escapes 0x7F and every non-ASCII character as \uXXXX
// (surrogate pairs for characters outside the BMP), as Dropbox requires for Dropbox-API-Arg.
func HeaderSafeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for _, r := range string(b) {
		switch {
		case r < 0x7F:
			sb.WriteRune(r)
		case r <= 0xFFFF:
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		}
	}
	return sb.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
