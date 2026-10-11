// Command demo starts the adapter binary and drives it over HTTP the way an
// agent would: connect with a token, find a recipe, inspect the pantry, view
// this week's meals, propose a grocery addition, write it after approval, read
// it back.
//
// By default it runs against the built-in fake Paprika. With -live it uses the
// account configured in the environment and really adds items to a grocery list.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pavel-alay/paprika-adapter/internal/paprikatest"
)

func main() {
	adapter := flag.String("adapter", "", "path to the paprika-adapter binary (required)")
	live := flag.Bool("live", false, "use the real Paprika account from the environment instead of the fake")
	yes := flag.Bool("yes", false, "approve the write without asking (fake only)")
	query := flag.String("query", "chicken", "recipe search text")
	listName := flag.String("list", "", "grocery list name to add to (default: the account's default list)")
	maxItems := flag.Int("max-items", 3, "propose at most this many missing ingredients")
	raw := flag.Bool("raw", false, "also print every tool result as JSON")
	flag.Parse()
	if *adapter == "" || (*live && *yes) {
		fmt.Fprintln(os.Stderr, "usage: demo -adapter PATH [-live | -yes] [-query TEXT] [-list NAME]")
		os.Exit(2)
	}
	if err := run(*adapter, *live, *yes, *raw, *query, *listName, *maxItems); err != nil {
		fmt.Fprintln(os.Stderr, "demo failed:", err)
		os.Exit(1)
	}
}

func run(adapter string, live, yes, raw bool, query, listName string, maxItems int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	token, err := exec.Command(adapter, "token").Output()
	if err != nil {
		return fmt.Errorf("make a token: %w", err)
	}
	bearer := strings.TrimSpace(string(token))
	addr, err := freeAddr()
	if err != nil {
		return err
	}
	cmd := exec.Command(adapter, "serve")
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "PAPRIKA_ENABLE_WRITES=true", "PAPRIKA_LISTEN="+addr, "PAPRIKA_AUTH_TOKENS=demo:"+bearer)
	if !live {
		fake := paprikatest.New(time.Now())
		fake.Listen()
		defer fake.Close()
		dir, err := os.MkdirTemp("", "paprika-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		cmd.Env = append(cmd.Env, "PAPRIKA_BASE_URL="+fake.URL, "PAPRIKA_EMAIL="+paprikatest.Email,
			"PAPRIKA_PASSWORD="+paprikatest.Password, "PAPRIKA_DATA_DIR="+dir, "PAPRIKA_LOG_LEVEL=warn")
		defer func() {
			fmt.Printf("\nFake Paprika saw %d grocery write request(s) in total.\n", fake.Hits("POST /api/v2/sync/groceries/"))
		}()
		fmt.Println("Backend: built-in fake Paprika (synthetic fixtures), not a real account.")
	} else {
		fmt.Println("Backend: LIVE Paprika account from the environment.")
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start adapter: %w", err)
	}
	stop := func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}
	defer stop()
	endpoint := "http://" + addr + "/mcp"
	if err := waitForPort(ctx, addr); err != nil {
		return err
	}

	fmt.Printf("\n== Step 0: Connect to %s\n", endpoint)
	for _, attempt := range []struct{ label, header string }{
		{"no token", ""},
		{"wrong token", "Bearer " + strings.Repeat("0", len(bearer))},
	} {
		status, err := probe(ctx, endpoint, attempt.header)
		if err != nil {
			return err
		}
		fmt.Printf("    %-12s -> HTTP %d\n", attempt.label, status)
		if status != http.StatusUnauthorized {
			return fmt.Errorf("a request with %s was not refused", attempt.label)
		}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "paprika-demo", Version: "1"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			fmt.Printf("\n>>> APPROVAL REQUEST FROM THE ADAPTER\n%s\n", indent(req.Params.Message))
			if yes {
				fmt.Println(">>> approved automatically (-yes)")
				return &mcp.ElicitResult{Action: "accept"}, nil
			}
			fmt.Print(">>> approve? [y/N] ")
			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.EqualFold(strings.TrimSpace(answer), "y") {
				return &mcp.ElicitResult{Action: "accept"}, nil
			}
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint,
		HTTPClient: &http.Client{Transport: withToken(bearer)}}, nil)
	if err != nil {
		return fmt.Errorf("connect to adapter: %w", err)
	}
	defer session.Close()
	fmt.Println("    demo's token -> connected")
	d := &demo{ctx: ctx, session: session, raw: raw, stop: stop}
	if err := d.waitForFirstSync(); err != nil {
		return err
	}

	d.step(1, "Find a recipe")
	found := d.call("search_recipes", map[string]any{"query": query, "limit": 3})
	recipes := list(found, "recipes")
	if len(recipes) == 0 {
		return fmt.Errorf("no recipe matches %q", query)
	}
	for _, r := range recipes {
		fmt.Printf("    %s  (uid %s)\n", r["name"], r["uid"])
	}
	recipe := d.call("get_recipe", map[string]any{"uid": recipes[0]["uid"]})["recipe"].(map[string]any)
	fmt.Printf("    ingredients_raw of %q:\n%s\n", recipe["name"], indent(recipe["ingredients_raw"].(string)))

	d.step(2, "Inspect the pantry")
	pantry := list(d.call("list_pantry", map[string]any{"in_stock_only": true}), "items")
	var have []string
	for _, p := range pantry {
		have = append(have, strings.ToLower(p["ingredient"].(string)))
	}
	fmt.Printf("    in stock: %s\n", strings.Join(have, ", "))

	d.step(3, "View this week's meals")
	plan := d.call("get_meal_plan", nil)
	fmt.Printf("    week %s to %s\n", plan["from"], plan["to"])
	for _, m := range list(plan, "meals") {
		fmt.Printf("    %s  %-9s %s\n", m["date"], m["meal_type"], m["name"])
	}

	d.step(4, "Propose a grocery addition (nothing is written yet)")
	var target map[string]any
	for _, l := range list(d.call("list_grocery_lists", nil), "lists") {
		if (listName == "" && l["is_default"] == true) || strings.EqualFold(listName, l["name"].(string)) {
			target = l
		}
	}
	if target == nil {
		return fmt.Errorf("grocery list %q not found", listName)
	}
	var missing []any
	for _, line := range list(recipe, "ingredients_parsed") {
		item, _ := line["item"].(string)
		if item == "" || line["heading"] == true || inPantry(have, item) || len(missing) == maxItems {
			continue
		}
		qty := strings.TrimSpace(fmt.Sprint(line["quantity"], " ", line["unit"]))
		missing = append(missing, map[string]any{"name": item, "quantity": strings.TrimSpace(strings.ReplaceAll(qty, "<nil>", ""))})
	}
	if len(missing) == 0 {
		return fmt.Errorf("the pantry already covers %q; try another -query", recipe["name"])
	}
	proposed := d.call("propose_grocery_items", map[string]any{"list_uid": target["uid"], "items": missing})
	proposal := proposed["proposal"].(map[string]any)
	fmt.Printf("    proposal_id: %s\n    summary: %s\n", proposal["proposal_id"], proposal["summary"])

	d.step(5, "Write it, with approval")
	committed := d.call("commit_proposal", map[string]any{"proposal_id": proposal["proposal_id"], "confirm": true})
	fmt.Printf("    status: %s, verified by read-back: %v\n", committed["status"], committed["verified"])
	for _, it := range list(committed, "items") {
		fmt.Printf("    written: %s  (uid %s)\n", it["name"], it["uid"])
	}
	again := d.call("commit_proposal", map[string]any{"proposal_id": proposal["proposal_id"], "confirm": true})
	fmt.Printf("    same commit repeated: status %s (no second write)\n", again["status"])

	d.step(6, "Read it back")
	after := d.call("list_grocery_items", map[string]any{"list_uid": target["uid"]})
	for _, it := range list(after, "items") {
		fmt.Printf("    %s\n", it["name"])
	}
	for _, f := range list(after, "sync") {
		fmt.Printf("    sync: %s is %s (last synced %s)\n", f["collection"], f["state"], f["last_synced_at"])
	}
	return nil
}

// withToken adds the client's bearer token to every request.
type withToken string

func (t withToken) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(t))
	return http.DefaultTransport.RoundTrip(r)
}

// probe sends an MCP initialize with the given Authorization header and returns the HTTP status.
func probe(ctx context.Context, endpoint, authorization string) (int, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

// freeAddr picks a loopback port that is free right now.
func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

func waitForPort(ctx context.Context, addr string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the adapter did not start listening on %s", addr)
}

type demo struct {
	ctx     context.Context
	session *mcp.ClientSession
	raw     bool
	stop    func() // ends the adapter process
}

// waitForFirstSync blocks until every collection has a complete cached copy.
// On a real account the first recipe download takes longer than one tool call may.
func (d *demo) waitForFirstSync() error {
	announced := false
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		res, err := d.session.CallTool(d.ctx, &mcp.CallToolParams{Name: "sync_status"})
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var status map[string]any
		_ = json.Unmarshal(raw, &status)
		pending := ""
		for _, f := range list(status, "sync") {
			if f["state"] != "never_synced" {
				continue
			}
			pending = fmt.Sprintf("%s (%v rows so far)", f["collection"], f["rows"])
			if e, ok := f["error"].(map[string]any); ok && e["code"] != "upstream_unavailable" {
				return fmt.Errorf("first sync of %s failed: %v: %v", f["collection"], e["code"], e["message"])
			}
		}
		if pending == "" {
			return nil
		}
		if !announced {
			fmt.Println("    waiting for the adapter's first sync with Paprika ...")
			announced = true
		}
		fmt.Printf("    still syncing: %s\n", pending)
	}
	return fmt.Errorf("the first sync did not finish within 5 minutes")
}

func (d *demo) step(n int, title string) { fmt.Printf("\n== Step %d: %s\n", n, title) }

// call invokes a tool, prints the call, and returns the structured result.
func (d *demo) call(name string, args map[string]any) map[string]any {
	in, _ := json.Marshal(args)
	fmt.Printf("  -> %s %s\n", name, in)
	res, err := d.session.CallTool(d.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		d.stop()
		fmt.Fprintln(os.Stderr, "demo failed:", err)
		os.Exit(1)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if d.raw {
		pretty, _ := json.MarshalIndent(out, "     ", "  ")
		fmt.Printf("  <- %s\n", pretty)
	}
	if out["ok"] != true {
		d.stop()
		fmt.Fprintf(os.Stderr, "demo failed: %s returned %s\n", name, raw)
		os.Exit(1)
	}
	return out
}

func list(obj map[string]any, key string) []map[string]any {
	items, _ := obj[key].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func inPantry(have []string, item string) bool {
	item = strings.ToLower(item)
	for _, h := range have {
		if strings.Contains(item, h) || strings.Contains(h, item) {
			return true
		}
	}
	return false
}

func indent(text string) string { return "      " + strings.ReplaceAll(text, "\n", "\n      ") }
