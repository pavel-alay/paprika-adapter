package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
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
	defaultList = "9E12FCF54A89FC52EA8E1C5DA1BDA62A6617ED8BDC2AEB6F291B93C7A399F6F6"
	costcoList  = "90ACAF2C-B0E9-4D46-A50A-3F00C9DF360D"
	dinner      = "216713D08860CFA0D9787EA5C6CEBC8A8F5B73777F91C904853AC234BB9DF642"
	roastUID    = "014031E3-BC74-4CA8-B345-36EBE9C9794B"
	soupUID     = "DF8BD763-2ADA-49E2-AA2C-05A47A81B007-6735-000003ABB738847D"
	trashedUID  = "C9A07E35-2F6D-4B18-A3C4-8D5E1B9F0672"
)

// harness is a real MCP client talking to the real server over an in-memory
// transport, with the fake Paprika behind the real client and a real cache.
type harness struct {
	t       *testing.T
	fake    *paprikatest.Server
	session *mcp.ClientSession
	now     time.Time // Thursday 2026-10-08; the week is Mon 5th to Sun 11th
	logs    bytes.Buffer
	replies bytes.Buffer // every tool reply, as the agent would see it

	approval string   // how the user answers an approval prompt; "" = the client cannot prompt
	prompts  []string // approval prompts shown to the user
	human    bool     // server insists on a prompt
	protocol string   // MCP protocol revision the client speaks; "" = newest
}

func newHarness(t *testing.T, writes bool, opts ...func(*harness)) *harness {
	t.Helper()
	h := &harness{t: t, now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	for _, o := range opts {
		o(h)
	}
	h.fake = paprikatest.New(h.now)
	clock := func() time.Time { return h.now }
	log := slog.New(slog.NewTextHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client, err := paprika.New(paprika.Options{BaseURL: h.fake.URL, Email: paprikatest.Email, Password: paprikatest.Password,
		HTTPClient: h.fake.Client(), RetryWait: time.Millisecond, Logger: log,
		TokenFile: filepath.Join(t.TempDir(), "token")})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	sy := syncer.New(client, st, log)
	sy.Now = clock
	srv := mcpserver.New(mcpserver.Deps{Store: st, Sync: sy, Client: client, WritesEnabled: writes,
		Location: time.UTC, Now: clock, Version: "test", RequireHumanApproval: h.human})
	var clientOpts *mcp.ClientOptions
	if h.approval != "" {
		clientOpts = &mcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			h.prompts = append(h.prompts, req.Params.Message)
			return &mcp.ElicitResult{Action: h.approval}, nil
		}}
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatal(err)
	}
	h.session, err = mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, clientOpts).Connect(ctx, cliT, &mcp.ClientSessionOptions{ProtocolVersion: h.protocol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.session.Close()
		st.Close()
	})
	return h
}

// reply is the union of the fields the tests look at across all tool outputs.
type reply struct {
	IsError bool `json:"-"`
	OK      bool `json:"ok"`
	Error   *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
	Sync []syncer.Freshness `json:"sync"`

	Recipes []struct {
		UID, Name  string
		Categories []string
		InTrash    bool `json:"in_trash"`
	} `json:"recipes"`
	TotalMatches int `json:"total_matches"`
	Truncated    bool
	Recipe       *struct {
		UID, Name         string
		IngredientsRaw    string `json:"ingredients_raw"`
		IngredientsParsed []struct {
			Raw, Quantity, Unit, Item, Note string
			Heading                         bool
		} `json:"ingredients_parsed"`
		Categories []mcpserver.NamedRef
		CachedAt   string `json:"cached_at"`
	} `json:"recipe"`
	Total     int
	Items     []item
	Lists     []mcpserver.GroceryListEntry
	List      *mcpserver.NamedRef
	From, To  string
	Today     string
	Meals     []mcpserver.MealEntry
	MealTypes []mcpserver.NamedRef `json:"meal_types"`
	Meal      *mcpserver.MealEntry

	Healthy       bool
	WritesEnabled bool `json:"writes_enabled"`

	Proposal *mcpserver.ProposalView
	Status   string
	Verified bool
	Skipped  []mcpserver.SkippedItem
}

// item covers pantry and grocery rows.
type item struct {
	UID, Name, Ingredient, Quantity, Aisle string
	InStock                                bool   `json:"in_stock"`
	ExpirationDate                         string `json:"expiration_date"`
	Purchased                              bool
}

func (h *harness) call(name string, args map[string]any) reply {
	h.t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: protocol error: %v", name, err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			h.replies.WriteString(text.Text)
		}
	}
	h.replies.Write(raw)
	var r reply
	if res.StructuredContent == nil {
		h.t.Fatalf("%s: no structured content; text: %s", name, h.replies.String())
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		h.t.Fatalf("%s: %v in %s", name, err, raw)
	}
	r.IsError = res.IsError
	if r.IsError == r.OK || (r.Error == nil) != r.OK {
		h.t.Fatalf("%s: inconsistent result: isError=%v ok=%v error=%v", name, r.IsError, r.OK, r.Error)
	}
	return r
}

// ok calls a tool and fails the test unless it succeeds.
func (h *harness) ok(name string, args map[string]any) reply {
	h.t.Helper()
	r := h.call(name, args)
	if !r.OK {
		h.t.Fatalf("%s failed: %s: %s", name, r.Error.Code, r.Error.Message)
	}
	return r
}

// fails calls a tool and fails the test unless it reports the given error code.
func (h *harness) fails(name string, args map[string]any, code string) reply {
	h.t.Helper()
	r := h.call(name, args)
	if r.OK || r.Error.Code != code {
		h.t.Fatalf("%s: got ok=%v error=%+v, want error code %s", name, r.OK, r.Error, code)
	}
	return r
}

func (h *harness) posts(collection string) int {
	return h.fake.Hits("POST /api/v2/sync/" + collection + "/")
}

func (h *harness) serverRows(collection, field string, value any) int {
	n := 0
	for _, r := range h.fake.Rows(collection) {
		if r[field] == value {
			n++
		}
	}
	return n
}
