// Command demo drives the adapter binary over stdio the way an agent would:
// find a recipe, inspect the pantry, view this week's meals, propose a grocery
// addition, write it after approval, read it back.
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
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"paprika-adapter/internal/paprikatest"
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
	cmd := exec.Command(adapter, "serve")
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "PAPRIKA_ENABLE_WRITES=true")
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
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("start adapter: %w", err)
	}
	defer session.Close()
	d := &demo{ctx: ctx, session: session, raw: raw}

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

type demo struct {
	ctx     context.Context
	session *mcp.ClientSession
	raw     bool
}

func (d *demo) step(n int, title string) { fmt.Printf("\n== Step %d: %s\n", n, title) }

// call invokes a tool, prints the call, and returns the structured result.
func (d *demo) call(name string, args map[string]any) map[string]any {
	in, _ := json.Marshal(args)
	fmt.Printf("  -> %s %s\n", name, in)
	res, err := d.session.CallTool(d.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
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
