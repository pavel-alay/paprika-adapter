package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"paprika-adapter/internal/mcpserver"
)

const (
	mcpPath     = "/mcp"
	minTokenLen = 32
)

// clientToken is one MCP client allowed to connect. Only the hash of its token is kept.
type clientToken struct {
	name string
	sum  [sha256.Size]byte
}

var clientName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// parseTokens reads PAPRIKA_AUTH_TOKENS: "name:token" entries separated by commas or whitespace.
func parseTokens(raw string) ([]clientToken, error) {
	var out []clientToken
	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	for i, entry := range entries {
		name, token, ok := strings.Cut(entry, ":")
		if !ok || !clientName.MatchString(name) {
			return nil, fmt.Errorf("PAPRIKA_AUTH_TOKENS: entry %d must look like name:token", i+1)
		}
		if len(token) < minTokenLen {
			return nil, fmt.Errorf("PAPRIKA_AUTH_TOKENS: the token of %q is shorter than %d characters; make one with: paprika-adapter token", name, minTokenLen)
		}
		sum := sha256.Sum256([]byte(token))
		for _, c := range out {
			if c.name == name || c.sum == sum {
				return nil, fmt.Errorf("PAPRIKA_AUTH_TOKENS: %q repeats a name or a token of %q", name, c.name)
			}
		}
		out = append(out, clientToken{name: name, sum: sum})
	}
	return out, nil
}

// printToken prints 256 random bits, to be used as the token of one client.
func printToken(w io.Writer) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, hex.EncodeToString(raw))
	return err
}

func serve(ctx context.Context) error {
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer a.store.Close()
	if len(a.cfg.clients) == 0 {
		return errors.New("PAPRIKA_AUTH_TOKENS must hold at least one name:token entry; make a token with: paprika-adapter token")
	}
	ln, err := net.Listen("tcp", a.cfg.listen)
	if err != nil {
		return err
	}
	var names []string
	for _, c := range a.cfg.clients {
		names = append(names, c.name)
	}
	a.log.Info("paprika-adapter starting", "version", version, "listen", ln.Addr().String(), "path", mcpPath, "clients", names,
		"writes_enabled", a.cfg.writes, "approval", a.cfg.approval, "paprika_host", a.client.Host(), "data_dir", a.cfg.dataDir)
	if a.cfg.syncInterval > 0 {
		go a.sync.Run(ctx, a.cfg.syncInterval)
	}
	mcpSrv := mcpserver.New(a.deps())
	mcpSrv.AddReceivingMiddleware(logCalls(a.log))
	srv := &http.Server{Handler: newHandler(mcpSrv, a.cfg.clients, a.log), ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	// Open event streams never go idle, so they are cut after a short grace period.
	grace, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(grace); err != nil {
		srv.Close()
	}
	// A sync that was cut short records where it stopped; let it, before the cache closes.
	idle, cancelIdle := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelIdle()
	a.sync.Stop(idle)
	return nil
}

// newHandler serves MCP at mcpPath to requests carrying a known bearer token.
// The client's name becomes the session's user, so one client cannot use another's session.
func newHandler(srv *mcp.Server, clients []clientToken, log *slog.Logger) http.Handler {
	verify := func(_ context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
		sum := sha256.Sum256([]byte(token))
		name := ""
		for _, c := range clients {
			if subtle.ConstantTimeCompare(sum[:], c.sum[:]) == 1 {
				name = c.name
			}
		}
		if name == "" {
			log.Warn("mcp request with an unknown token rejected", "remote", r.RemoteAddr)
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: name}, nil
	}
	guard := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	mux := http.NewServeMux()
	mux.Handle(mcpPath, guard(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)))
	return mux
}

// logCalls records which client called which tool: the write tools at info, reads at debug.
func logCalls(log *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return res, err
			}
			client := ""
			if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
				client = extra.TokenInfo.UserID
			}
			outcome := "ok"
			if out, _ := res.(*mcp.CallToolResult); err != nil || (out != nil && out.IsError) {
				outcome = "error"
			} else if out != nil && out.InputRequests != nil {
				outcome = "awaiting_approval"
			}
			level := slog.LevelDebug
			if strings.HasPrefix(params.Name, "propose_") || params.Name == "commit_proposal" {
				level = slog.LevelInfo
			}
			log.Log(ctx, level, "tool call", "tool", params.Name, "client", client, "outcome", outcome)
			return res, err
		}
	}
}
