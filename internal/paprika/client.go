// Package paprika is a client for the unofficial Paprika 3 cloud sync API (v2 sync
// endpoints plus the v1 login). It knows nothing about the cache or MCP.
package paprika

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL = "https://www.paprikaapp.com"

	loginPath = "/api/v1/account/login/"
	syncPath  = "/api/v2/sync/"

	maxResponseBytes = 64 << 20
	readAttempts     = 3
	authCooldown     = 5 * time.Minute
)

// Options configures a Client. Email and Password are required.
type Options struct {
	BaseURL    string
	Email      string
	Password   string
	TokenFile  string // optional; the bearer token is cached here with mode 0600
	HTTPClient *http.Client
	UserAgent  string
	Logger     *slog.Logger
	RetryWait  time.Duration // base backoff between read retries, default 500ms
}

// Client talks to Paprika. It is safe for concurrent use.
type Client struct {
	base      string
	email     string
	password  string
	tokenFile string
	http      *http.Client
	userAgent string
	log       *slog.Logger
	retryWait time.Duration

	mu           sync.Mutex
	token        string
	authErr      *Error // last credential rejection, reused during authCooldown
	authFailedAt time.Time
}

// New validates opts and returns a Client. It performs no network I/O.
func New(opts Options) (*Client, error) {
	if opts.Email == "" || opts.Password == "" {
		return nil, &Error{Kind: KindInvalid, Op: "configure", Message: "Paprika email and password are required"}
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if err := checkBaseURL(base); err != nil {
		return nil, err
	}
	c := &Client{
		base: base, email: opts.Email, password: opts.Password, tokenFile: opts.TokenFile,
		http: opts.HTTPClient, userAgent: opts.UserAgent, log: opts.Logger, retryWait: opts.RetryWait,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.userAgent == "" {
		c.userAgent = "paprika-adapter"
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.retryWait <= 0 {
		c.retryWait = 500 * time.Millisecond
	}
	return c, nil
}

// checkBaseURL refuses to send credentials over plain HTTP anywhere but loopback.
func checkBaseURL(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return &Error{Kind: KindInvalid, Op: "configure", Message: "base URL is not a valid absolute URL"}
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); u.Scheme == "http" && (host == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return &Error{Kind: KindInvalid, Op: "configure", Message: "base URL must use https (plain http is allowed for loopback only)"}
}

// Host returns the host the client talks to, for diagnostics.
func (c *Client) Host() string {
	u, _ := url.Parse(c.base)
	return u.Host
}

// Status returns Paprika's per-collection change counters.
func (c *Client) Status(ctx context.Context) (map[string]int64, error) {
	op := "GET " + syncPath + "status/"
	res, err := c.call(ctx, http.MethodGet, syncPath+"status/", nil)
	if err != nil {
		return nil, err
	}
	var counters map[string]int64
	if err := json.Unmarshal(res, &counters); err != nil || len(counters) == 0 {
		return nil, &Error{Kind: KindProtocol, Op: op, Status: http.StatusOK, Message: "status is not an object of counters"}
	}
	return counters, nil
}

// List returns every row of a collection.
func (c *Client) List(ctx context.Context, col Collection) ([]Raw, error) {
	path := syncPath + string(col) + "/"
	res, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return decodeRows("GET "+path, res)
}

// RecipeRefs returns the uid and content hash of every recipe in the account.
func (c *Client) RecipeRefs(ctx context.Context) ([]RecipeRef, error) {
	path := syncPath + "recipes/"
	res, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var refs []RecipeRef
	if err := json.Unmarshal(res, &refs); err != nil {
		return nil, &Error{Kind: KindProtocol, Op: "GET " + path, Status: http.StatusOK, Message: "recipe list is not an array of {uid, hash}"}
	}
	for _, r := range refs {
		if r.UID == "" || r.Hash == "" {
			return nil, &Error{Kind: KindProtocol, Op: "GET " + path, Status: http.StatusOK, Message: "recipe list row without uid or hash"}
		}
	}
	return refs, nil
}

// Recipe fetches one full recipe.
func (c *Client) Recipe(ctx context.Context, uid string) (Raw, error) {
	path := syncPath + "recipe/" + url.PathEscape(uid) + "/"
	res, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Raw{}, err
	}
	rows, err := decodeRows("GET "+syncPath+"recipe/{uid}/", append(append([]byte("["), res...), ']'))
	if err != nil {
		return Raw{}, err
	}
	if rows[0].UID != uid {
		return Raw{}, &Error{Kind: KindProtocol, Op: "GET " + syncPath + "recipe/{uid}/", Status: http.StatusOK, Message: "recipe uid does not match the requested uid"}
	}
	return rows[0], nil
}

func decodeRows(op string, res json.RawMessage) ([]Raw, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(res, &items); err != nil {
		return nil, &Error{Kind: KindProtocol, Op: op, Status: http.StatusOK, Message: "result is not an array"}
	}
	rows := make([]Raw, 0, len(items))
	for _, it := range items {
		var id struct {
			UID string `json:"uid"`
		}
		if err := json.Unmarshal(it, &id); err != nil || id.UID == "" {
			return nil, &Error{Kind: KindProtocol, Op: op, Status: http.StatusOK, Message: "row without a string uid"}
		}
		rows = append(rows, Raw{UID: id.UID, JSON: it})
	}
	return rows, nil
}

type body struct {
	contentType string
	data        []byte
}

type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// call performs one authenticated API call and returns the envelope's result.
// Reads are retried on transient failures; writes are sent at most once per call
// because the caller decides whether a repeat is safe. A 401 triggers exactly one
// re-login.
func (c *Client) call(ctx context.Context, method, path string, b *body) (json.RawMessage, error) {
	attempts := 1
	if method == http.MethodGet {
		attempts = readAttempts
	}
	relogged := false
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, c.retryWait*time.Duration(1<<(attempt-2))); err != nil {
				return nil, last
			}
		}
		token, err := c.currentToken(ctx)
		if err != nil {
			return nil, err
		}
		res, err := c.send(ctx, method, path, b, token)
		if err == nil {
			return res, nil
		}
		last = err
		pe := err.(*Error)
		if pe.Status == http.StatusUnauthorized && !relogged {
			relogged = true
			c.dropToken(token)
			attempt-- // the re-login does not consume a retry
			continue
		}
		if pe.Status == http.StatusUnauthorized {
			pe.Kind, pe.Message = KindAuth, "token refused again right after a successful login"
			return nil, pe
		}
		if !pe.Retryable() {
			return nil, pe
		}
	}
	return nil, last
}

func (c *Client) send(ctx context.Context, method, path string, b *body, token string) (json.RawMessage, error) {
	op := method + " " + opPath(path)
	var rd io.Reader
	if b != nil {
		rd = bytes.NewReader(b.data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, &Error{Kind: KindInvalid, Op: op, Message: "cannot build request"}
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if b != nil {
		req.Header.Set("Content-Type", b.contentType)
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Debug("paprika request failed", "op", op, "duration_ms", time.Since(start).Milliseconds())
		return nil, &Error{Kind: KindUnavailable, Op: op, Message: transportMessage(err), Err: err}
	}
	defer resp.Body.Close()
	raw, err := readBody(resp.Body)
	c.log.Debug("paprika request", "op", op, "status", resp.StatusCode, "bytes", len(raw), "duration_ms", time.Since(start).Milliseconds())
	if err != nil {
		return nil, &Error{Kind: KindUnavailable, Op: op, Status: resp.StatusCode, Message: "response body could not be read", Err: err}
	}

	var env envelope
	parsed := json.Unmarshal(raw, &env) == nil
	msg := http.StatusText(resp.StatusCode)
	if parsed && env.Error != nil && env.Error.Message != "" {
		msg = env.Error.Message
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, &Error{Kind: KindAuth, Op: op, Status: resp.StatusCode, Message: msg}
	case resp.StatusCode == http.StatusNotFound:
		return nil, &Error{Kind: KindNotFound, Op: op, Status: resp.StatusCode, Message: msg}
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return nil, &Error{Kind: KindUnavailable, Op: op, Status: resp.StatusCode, Message: msg}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &Error{Kind: KindRejected, Op: op, Status: resp.StatusCode, Message: msg}
	case !parsed:
		return nil, &Error{Kind: KindProtocol, Op: op, Status: resp.StatusCode, Message: "response is not JSON"}
	case env.Error != nil:
		// Paprika reports many application errors as HTTP 200 with an error body.
		kind := KindRejected
		if strings.Contains(strings.ToLower(msg), "not found") {
			kind = KindNotFound
		}
		return nil, &Error{Kind: kind, Op: op, Status: resp.StatusCode, Message: msg}
	case env.Result == nil:
		return nil, &Error{Kind: KindProtocol, Op: op, Status: resp.StatusCode, Message: "response has neither result nor error"}
	}
	return env.Result, nil
}

// opPath hides entity uids so error messages and logs stay uniform.
func opPath(path string) string {
	if rest, ok := strings.CutPrefix(path, syncPath+"recipe/"); ok && rest != "" {
		return syncPath + "recipe/{uid}/"
	}
	if rest, ok := strings.CutPrefix(path, syncPath+"photo/"); ok && rest != "" {
		return syncPath + "photo/{uid}/"
	}
	return path
}

func transportMessage(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "request timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "request cancelled"
	}
	return "network error reaching Paprika"
}

func readBody(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("response larger than %d bytes", maxResponseBytes)
	}
	// Some responses arrive gzipped without a Content-Encoding header.
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(io.LimitReader(zr, maxResponseBytes))
	}
	return raw, nil
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
