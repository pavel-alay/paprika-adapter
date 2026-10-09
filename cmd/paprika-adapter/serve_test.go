package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"paprika-adapter/internal/mcpserver"
	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/paprikatest"
	"paprika-adapter/internal/store"
	"paprika-adapter/internal/syncer"
)

const (
	annaToken   = "anna-token-0123456789abcdef0123456789abcdef"
	peterToken  = "peter-token-0123456789abcdef0123456789abcdef"
	defaultList = "9E12FCF54A89FC52EA8E1C5DA1BDA62A6617ED8BDC2AEB6F291B93C7A399F6F6"
)

// httpStack is the real HTTP handler in front of the real MCP server, with the
// fake Paprika behind it and a real cache.
type httpStack struct {
	handler http.Handler
	fake    *paprikatest.Server
	logs    *lockedBuffer
}

func newHTTPStack(t *testing.T) *httpStack {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s := &httpStack{fake: paprikatest.New(now), logs: &lockedBuffer{}}
	log := slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client, err := paprika.New(paprika.Options{BaseURL: s.fake.URL, Email: paprikatest.Email, Password: paprikatest.Password,
		HTTPClient: s.fake.Client(), RetryWait: time.Millisecond, Logger: log, TokenFile: filepath.Join(t.TempDir(), "token")})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sy := syncer.New(client, st, log)
	sy.Now = func() time.Time { return now }
	srv := mcpserver.New(mcpserver.Deps{Store: st, Sync: sy, Client: client, WritesEnabled: true, Location: time.UTC, Now: sy.Now, Version: "test"})
	srv.AddReceivingMiddleware(logCalls(log))
	clients, err := parseTokens("anna:" + annaToken + ",peter:" + peterToken)
	if err != nil {
		t.Fatal(err)
	}
	s.handler = newHandler(srv, clients, log)
	return s
}

// post sends one JSON-RPC message the way an MCP client would and returns the buffered response.
func (s *httpStack) post(authorization, session, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+mcpPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
		r.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

func TestHTTPRejectsRequestsWithoutAKnownToken(t *testing.T) {
	s := newHTTPStack(t)
	for name, authorization := range map[string]string{
		"no header":       "",
		"unknown token":   "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef",
		"token prefix":    "Bearer " + annaToken[:len(annaToken)-1],
		"wrong scheme":    "Basic " + annaToken,
		"token as scheme": annaToken,
	} {
		t.Run(name, func(t *testing.T) {
			rec := s.post(authorization, "", initialize)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "serverInfo") || rec.Header().Get("Mcp-Session-Id") != "" {
				t.Fatalf("an unauthenticated request reached the MCP server: %s", rec.Body)
			}
		})
	}
	if rec := s.post("Bearer "+annaToken, "", initialize); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "serverInfo") {
		t.Fatalf("a known token was refused: %d %s", rec.Code, rec.Body)
	}
	other := httptest.NewRecorder()
	s.handler.ServeHTTP(other, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/", nil))
	if other.Code != http.StatusNotFound {
		t.Fatalf("a path other than %s answered %d", mcpPath, other.Code)
	}
	if logs := s.logs.String(); strings.Contains(logs, annaToken) || strings.Contains(logs, annaToken[:20]) || !strings.Contains(logs, "unknown token rejected") {
		t.Fatalf("logs leak a token or miss the rejection:\n%s", logs)
	}
}

func TestHTTPSessionBelongsToTheClientThatOpenedIt(t *testing.T) {
	s := newHTTPStack(t)
	opened := s.post("Bearer "+annaToken, "", initialize)
	session := opened.Header().Get("Mcp-Session-Id")
	if opened.Code != http.StatusOK || session == "" {
		t.Fatalf("initialize: %d, session %q", opened.Code, session)
	}
	const listTools = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	if rec := s.post("Bearer "+peterToken, session, listTools); rec.Code != http.StatusForbidden {
		t.Fatalf("another client used the session: %d %s", rec.Code, rec.Body)
	}
	if rec := s.post("Bearer "+annaToken, session, listTools); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "search_recipes") {
		t.Fatalf("the owner could not use its session: %d %s", rec.Code, rec.Body)
	}
}

// A real MCP client over the HTTP transport: reads, then a write approved by the user.
func TestHTTPClientReadsAndWritesWithApproval(t *testing.T) {
	for _, protocol := range []string{"2025-06-18", "2025-11-25", ""} {
		t.Run("protocol "+protocol, func(t *testing.T) {
			s := newHTTPStack(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var prompts []string
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
				ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
					prompts = append(prompts, req.Params.Message)
					return &mcp.ElicitResult{Action: "accept"}, nil
				},
			})
			transport := &mcp.StreamableClientTransport{Endpoint: "http://127.0.0.1:8080" + mcpPath, HTTPClient: streamClient(s.handler, annaToken)}
			session, err := client.Connect(ctx, transport, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()

			tools, err := session.ListTools(ctx, nil)
			if err != nil || len(tools.Tools) != 13 {
				t.Fatalf("tools = %v, err = %v", tools, err)
			}
			call := func(name string, args map[string]any) map[string]any {
				t.Helper()
				res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				raw, _ := json.Marshal(res.StructuredContent)
				var out map[string]any
				if err := json.Unmarshal(raw, &out); err != nil || out["ok"] != true {
					t.Fatalf("%s = %s", name, raw)
				}
				return out
			}
			if found := call("search_recipes", map[string]any{"query": "chicken"}); found["total_matches"] == float64(0) {
				t.Fatalf("search = %v", found)
			}
			proposed := call("propose_grocery_items", map[string]any{"list_uid": defaultList, "items": []any{map[string]any{"name": "oat milk"}}})
			id := proposed["proposal"].(map[string]any)["proposal_id"]
			committed := call("commit_proposal", map[string]any{"proposal_id": id, "confirm": true})
			if committed["status"] != "committed" || committed["verified"] != true || len(prompts) != 1 || s.fake.Hits("POST /api/v2/sync/groceries/") != 1 {
				t.Fatalf("commit = %v, prompts = %q", committed, prompts)
			}
			logs := s.logs.String()
			if !strings.Contains(logs, "tool=commit_proposal client=anna outcome=ok") || strings.Contains(logs, annaToken) {
				t.Fatalf("the write is not attributed to its client, or the token is logged:\n%s", logs)
			}
		})
	}
}

func TestHTTPClientWithAWrongTokenCannotConnect(t *testing.T) {
	s := newHTTPStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	transport := &mcp.StreamableClientTransport{Endpoint: "http://127.0.0.1:8080" + mcpPath,
		HTTPClient: streamClient(s.handler, strings.Repeat("x", 40))}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err == nil {
		session.Close()
		t.Fatal("connected with an unknown token")
	}
}

func TestParseTokens(t *testing.T) {
	clients, err := parseTokens(" anna:" + annaToken + ",\n peter:" + peterToken + "\n")
	if err != nil || len(clients) != 2 || clients[0].name != "anna" || clients[1].name != "peter" {
		t.Fatalf("clients = %+v, err = %v", clients, err)
	}
	if clients, err := parseTokens(""); err != nil || len(clients) != 0 {
		t.Fatalf("empty: %+v, %v", clients, err)
	}
	for name, raw := range map[string]string{
		"no name":        annaToken,
		"bad name":       "an na:" + annaToken,
		"short token":    "anna:hunter2hunter2",
		"repeated name":  "anna:" + annaToken + ",anna:" + peterToken,
		"repeated token": "anna:" + annaToken + ",peter:" + annaToken,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseTokens(raw)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), annaToken) || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("error echoes a token: %v", err)
			}
		})
	}
}

func TestPrintTokenIsLongEnoughToUse(t *testing.T) {
	var a, b bytes.Buffer
	if err := printToken(&a); err != nil {
		t.Fatal(err)
	}
	if err := printToken(&b); err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(a.String())
	if len(token) != 64 || a.String() == b.String() {
		t.Fatalf("tokens %q, %q", a.String(), b.String())
	}
	if _, err := parseTokens("anna:" + token); err != nil {
		t.Fatal(err)
	}
}

func TestServeRefusesToStartWithoutClientTokens(t *testing.T) {
	setEnv(t, "PAPRIKA_EMAIL", "a@b.c", "PAPRIKA_PASSWORD", "pw", "PAPRIKA_DATA_DIR", t.TempDir())
	err := serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "PAPRIKA_AUTH_TOKENS") {
		t.Fatalf("err = %v", err)
	}
}

// streamClient answers requests from h in-process, sending token as the bearer
// token. Unlike a recorder it hands the response over as soon as the headers
// are written, which event streams need.
func streamClient(h http.Handler, token string) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
		pr, pw := io.Pipe()
		w := &streamWriter{header: http.Header{}, body: pw, started: make(chan struct{})}
		go func() {
			h.ServeHTTP(w, r)
			w.WriteHeader(http.StatusOK)
			pw.Close()
		}()
		select {
		case <-w.started:
		case <-r.Context().Done():
			pr.Close()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: w.status, Status: http.StatusText(w.status), Header: w.sent, Body: pr, Request: r,
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, ContentLength: -1}, nil
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type streamWriter struct {
	header  http.Header
	sent    http.Header
	status  int
	body    *io.PipeWriter
	once    sync.Once
	started chan struct{}
}

func (w *streamWriter) Header() http.Header { return w.header }
func (w *streamWriter) Flush()              {}

func (w *streamWriter) WriteHeader(status int) {
	w.once.Do(func() {
		w.status, w.sent = status, w.header.Clone()
		close(w.started)
	})
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
