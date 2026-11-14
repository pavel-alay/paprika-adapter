package mcpserver_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pavel-alay/paprika-adapter/internal/mcpserver"
)

func withTools(names ...string) func(*harness) {
	return func(h *harness) { h.tools = names }
}

// The exported lists are what PAPRIKA_TOOLS is checked against, so they must
// name exactly the tools the server registers.
func TestToolListsMatchTheRegisteredTools(t *testing.T) {
	want := append(append(append([]string{}, mcpserver.ReadTools...), mcpserver.ProposeTools...), mcpserver.CommitTool)
	slices.Sort(want)
	if got := toolNames(t, newHarness(t, true)); !slices.Equal(got, want) {
		t.Fatalf("registered %v\n lists say %v", got, want)
	}
}

func TestAllowlistKeepsOnlyTheListedReadTools(t *testing.T) {
	got := strings.Join(toolNames(t, newHarness(t, true, withTools("list_pantry", "search_recipes"))), " ")
	if want := "list_pantry search_recipes sync_status"; got != want {
		t.Fatalf("tools = %s\n want  %s", got, want)
	}
}

func TestAllowlistAddsCommitForAProposeTool(t *testing.T) {
	got := strings.Join(toolNames(t, newHarness(t, true, withTools("list_pantry", "propose_pantry_items"))), " ")
	if want := "commit_proposal list_pantry propose_pantry_items sync_status"; got != want {
		t.Fatalf("tools = %s\n want  %s", got, want)
	}
}

func TestAllowlistCannotEnableWritesThatAreOff(t *testing.T) {
	got := strings.Join(toolNames(t, newHarness(t, false, withTools("list_pantry", "propose_pantry_items", "commit_proposal"))), " ")
	if want := "list_pantry sync_status"; got != want {
		t.Fatalf("tools = %s\n want  %s", got, want)
	}
}

func TestRemovedToolCannotBeCalled(t *testing.T) {
	h := newHarness(t, true, withTools("list_pantry", "propose_pantry_items"))
	if _, err := h.session.CallTool(t.Context(), &mcp.CallToolParams{Name: "propose_recipe_edit", Arguments: map[string]any{}}); err == nil {
		t.Fatal("a tool left out of the allowlist answered")
	}
}

func TestAllowlistWithoutWriteToolsDoesNotOfferWrites(t *testing.T) {
	h := newHarness(t, true, withTools("search_recipes"))
	if h.ok("sync_status", nil).WritesEnabled {
		t.Fatal("sync_status says writes are enabled, but no write tool is served")
	}
	if told := h.session.InitializeResult().Instructions; strings.Contains(told, "commit_proposal") {
		t.Fatalf("instructions describe writes: %s", told)
	}
}

// A proposal stored before the allowlist was narrowed must not be committed
// through a commit_proposal that is served for another propose tool.
func TestCommitRefusesAKindLeftOutOfTheAllowlist(t *testing.T) {
	full := newHarness(t, true)
	p := full.ok("propose_recipe_edit", map[string]any{"uid": roastUID, "rating": 3}).Proposal
	narrow := newHarness(t, true, withTools("propose_pantry_items"), func(h *harness) { h.cache = full.cache })
	narrow.fails("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}, "tool_not_allowed")
	if n := recipePosts(narrow, roastUID); n != 0 {
		t.Fatalf("recipe posts = %d, want 0", n)
	}
}

// A commit that already happened is reported as such, whatever the allowlist says now.
func TestRepeatedCommitIsAnsweredAfterTheAllowlistChanged(t *testing.T) {
	full := newHarness(t, true)
	p := full.ok("propose_recipe_edit", map[string]any{"uid": roastUID, "rating": 3}).Proposal
	full.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true})
	narrow := newHarness(t, true, withTools("propose_pantry_items"), func(h *harness) { h.cache = full.cache })
	if c := narrow.ok("commit_proposal", map[string]any{"proposal_id": p.ProposalID, "confirm": true}); c.Status != "already_committed" {
		t.Fatalf("status = %q, want already_committed", c.Status)
	}
}

func TestParseTools(t *testing.T) {
	got, err := mcpserver.ParseTools(" list_pantry,search_recipes\nlist_pantry ")
	if err != nil || !slices.Equal(got, []string{"list_pantry", "search_recipes"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := mcpserver.ParseTools(""); err != nil || got != nil {
		t.Fatalf("empty: got %v, %v; want nil (every tool)", got, err)
	}
	if _, err := mcpserver.ParseTools("list_pantry,propose_everything"); err == nil || !strings.Contains(err.Error(), "propose_everything") {
		t.Fatalf("unknown name: %v", err)
	}
	if got, err := mcpserver.ParseTools("list_pantry,\u00a0search_recipes\v"); err != nil || !slices.Equal(got, []string{"list_pantry", "search_recipes"}) {
		t.Fatalf("other whitespace: got %v, %v", got, err)
	}
	if _, err := mcpserver.ParseTools("list_pantry,commit_proposal"); err == nil {
		t.Fatal("commit_proposal without a propose tool was accepted")
	}
}
