package paprika

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// currentToken returns a bearer token, loading the cached one or logging in.
func (c *Client) currentToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	if t := c.readTokenFile(); t != "" {
		c.token = t
		return t, nil
	}
	// A rejected login is not retried for a while: hammering the login endpoint
	// with bad credentials helps nobody and may get the account throttled.
	if c.authErr != nil && time.Since(c.authFailedAt) < authCooldown {
		return "", c.authErr
	}
	token, err := c.login(ctx)
	if err != nil {
		if pe, ok := err.(*Error); ok && pe.Kind == KindAuth {
			c.authErr, c.authFailedAt = pe, time.Now()
		}
		return "", err
	}
	c.authErr = nil
	c.token = token
	c.writeTokenFile(token)
	return token, nil
}

// dropToken forgets a token the server refused, unless another call already replaced it.
func (c *Client) dropToken(refused string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != refused {
		return
	}
	c.token = ""
	if c.tokenFile != "" {
		_ = os.Remove(c.tokenFile)
	}
}

// Login forces a fresh login and reports whether the credentials work.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	c.token, c.authErr = token, nil
	c.writeTokenFile(token)
	return nil
}

// login exchanges email and password for a bearer token. Paprika answers a bad
// login with HTTP 200 and an error body, so the status code alone proves nothing.
func (c *Client) login(ctx context.Context) (string, error) {
	op := "POST " + loginPath
	form := url.Values{"email": {c.email}, "password": {c.password}}
	res, err := c.send(ctx, http.MethodPost, loginPath, &body{
		contentType: "application/x-www-form-urlencoded",
		data:        []byte(form.Encode()),
	}, "")
	if err != nil {
		pe := err.(*Error)
		if pe.Kind == KindRejected || pe.Kind == KindNotFound {
			pe.Kind = KindAuth
		}
		return "", pe
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(res, &out); err != nil || out.Token == "" {
		return "", &Error{Kind: KindProtocol, Op: op, Status: http.StatusOK, Message: "login response carries no token"}
	}
	c.log.Info("paprika login succeeded")
	return out.Token, nil
}

func (c *Client) readTokenFile() string {
	if c.tokenFile == "" {
		return ""
	}
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// writeTokenFile caches the token for the next process. Failure only costs an extra login.
func (c *Client) writeTokenFile(token string) {
	if c.tokenFile == "" {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.tokenFile), ".token-*")
	if err != nil {
		c.log.Warn("token cache not writable", "error", err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(token); err == nil {
		err = tmp.Chmod(0o600)
		if cerr := tmp.Close(); err == nil && cerr == nil {
			_ = os.Rename(tmp.Name(), c.tokenFile)
		}
	}
}
