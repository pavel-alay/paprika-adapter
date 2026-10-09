package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
)

type CommitIn struct {
	ProposalID string `json:"proposal_id" jsonschema:"id returned by propose_meal_plan_entry or propose_grocery_items"`
	Confirm    bool   `json:"confirm" jsonschema:"must be true, and only after the user has approved the proposal summary"`
}

// CommitResult is what a commit reports, and what is stored to answer repeats.
type CommitResult struct {
	ProposalID  string          `json:"proposal_id,omitempty"`
	Kind        string          `json:"kind,omitempty" jsonschema:"meal_plan_entry, grocery_items, pantry_items or recipe_edit"`
	Status      string          `json:"status,omitempty" jsonschema:"committed = written and verified by this call; already_committed = this proposal was committed before and nothing was written again"`
	Verified    bool            `json:"verified" jsonschema:"true when every written entity was read back from Paprika after the write"`
	CommittedAt string          `json:"committed_at,omitempty" jsonschema:"RFC 3339 UTC time of the verified commit"`
	Meal        *MealEntry      `json:"meal,omitempty" jsonschema:"the meal-plan entry as read back from Paprika"`
	List        *NamedRef       `json:"list,omitempty"`
	Items       []GroceryEntry  `json:"items,omitempty" jsonschema:"the grocery items as read back from Paprika"`
	Pantry      []PantryEntry   `json:"pantry_items,omitempty" jsonschema:"the pantry items as read back from Paprika"`
	Recipe      *RecipeEditView `json:"recipe_edit,omitempty" jsonschema:"the edited recipe: every changed field with the value it had before and the value read back from Paprika"`
	Skipped     []SkippedItem   `json:"skipped,omitempty" jsonschema:"proposed items that were not written, with the reason"`
}

type CommitOut struct {
	Meta
	CommitResult
}

const (
	statusCommitted        = "committed"
	statusAlreadyCommitted = "already_committed"
)

func (s *server) commitProposal(ctx context.Context, in CommitIn, out *CommitOut) *ToolError {
	id := strings.TrimSpace(in.ProposalID)
	if id == "" {
		return invalid("proposal_id is required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	prop, ok, err := s.Store.Proposal(ctx, id)
	if err != nil {
		return fromErr(err)
	}
	if !ok {
		return notFound("no proposal with id " + id + "; create one with a propose tool first")
	}
	if prop.State == store.ProposalCommitted {
		if err := json.Unmarshal(prop.Result, &out.CommitResult); err != nil {
			return fromErr(err)
		}
		out.Status = statusAlreadyCommitted
		return nil
	}
	if !in.Confirm {
		return &ToolError{Code: "confirmation_required", Message: "nothing was written: confirm must be true, and only after the user approved the proposal summary"}
	}
	if s.Now().After(prop.ExpiresAt) {
		return &ToolError{Code: "proposal_expired", Message: "nothing was written: the proposal expired at " + prop.ExpiresAt.UTC().Format(time.RFC3339) + "; propose again"}
	}
	var p payload
	if err := json.Unmarshal(prop.Payload, &p); err != nil {
		return fromErr(err)
	}
	terr := s.approve(ctx, p.Summary)
	if terr != nil {
		return terr
	}
	switch prop.Kind {
	case kindMeal:
		terr = s.commitMeal(ctx, p, out)
	case kindGroceries:
		terr = s.commitGroceries(ctx, p, out)
	case kindPantry:
		terr = s.commitPantry(ctx, p, out)
	case kindRecipe:
		terr = s.commitRecipeEdit(ctx, p, out)
	default:
		terr = invalid("proposal has an unknown kind")
	}
	if terr != nil {
		return terr
	}
	out.ProposalID, out.Kind, out.Status, out.Verified = id, prop.Kind, statusCommitted, true
	out.CommittedAt = s.Now().UTC().Format(time.RFC3339)
	// If this record is lost the next commit simply finds the rows in Paprika and
	// verifies them again, so a failure here must not turn a good write into an error.
	if res, err := json.Marshal(out.CommitResult); err == nil {
		_ = s.Store.CommitProposal(ctx, id, res)
	}
	return nil
}

const approvalID = "approval"

// approve asks the user, through the client, whether the write may happen. The
// first pass returns the question; the handler runs again once it is answered.
// A client that cannot ask is trusted to have asked itself, unless the server
// is configured to insist on a direct answer.
func (s *server) approve(ctx context.Context, summary string) *ToolError {
	c, _ := ctx.Value(callKey{}).(*call)
	if c == nil || !canElicit(c.req.Session) {
		if s.RequireHumanApproval {
			return &ToolError{Code: "approval_unavailable", Message: "nothing was written: this server writes only after the user approves through MCP elicitation, which this client does not offer"}
		}
		return nil
	}
	answer, answered := c.req.Params.InputResponses[approvalID]
	if !answered {
		c.ask = mcp.InputRequestMap{approvalID: &mcp.ElicitParams{
			Message:         "Approve this change to Paprika?\n\n" + summary,
			RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		}}
		return &ToolError{Code: "approval_pending", Message: "waiting for the user's approval"}
	}
	if res, ok := answer.(*mcp.ElicitResult); !ok || res.Action != "accept" {
		return &ToolError{Code: "approval_declined", Message: "nothing was written: the user did not approve the change"}
	}
	return nil
}

func canElicit(session *mcp.ServerSession) bool {
	if session == nil {
		return false
	}
	init := session.InitializeParams()
	return init != nil && init.Capabilities != nil && init.Capabilities.Elicitation != nil
}

// commitMeal follows the same steps as every write: read Paprika, skip what is
// already there, refuse what would overwrite or duplicate, write, read back.
func (s *server) commitMeal(ctx context.Context, p payload, out *CommitOut) *ToolError {
	want := *p.Meal
	meals, terr := s.pullMeals(ctx)
	if terr != nil {
		return terr
	}
	if _, present := findMeal(meals, want.UID); !present {
		if dup := duplicateMeal(meals, want); dup != nil && !p.AllowDuplicates {
			return &ToolError{Code: "duplicate", Message: "nothing was written: the same entry was added to the plan in the meantime (meal uid " + dup.UID + ")"}
		}
		if err := s.Client.AddMeal(ctx, want); err != nil {
			return writeFailed(err)
		}
		if meals, terr = s.pullMeals(ctx); terr != nil {
			return notVerified(terr.Message)
		}
	}
	got, present := findMeal(meals, want.UID)
	if !present {
		return notVerified("Paprika accepted the entry but does not return it yet")
	}
	if got.Name != want.Name || dateOnly(got.Date) != dateOnly(want.Date) || deref(got.RecipeUID) != deref(want.RecipeUID) {
		return &ToolError{Code: "uid_conflict", Message: "nothing was overwritten: Paprika already holds a different meal under uid " + want.UID + "; propose again"}
	}
	types, err := s.mealTypes(ctx)
	if err != nil {
		return fromErr(err)
	}
	entry := types.entry(got)
	out.Meal = &entry
	out.Sync = s.Sync.Freshness(ctx, string(paprika.Meals))
	return nil
}

func (s *server) commitGroceries(ctx context.Context, p payload, out *CommitOut) *ToolError {
	items, terr := s.pullGroceries(ctx)
	if terr != nil {
		return terr
	}
	skipped := p.Skipped
	var expect, send []paprika.NewGroceryItem
	for _, it := range p.Items {
		if _, present := findGrocery(items, it.UID); present {
			expect = append(expect, it)
			continue
		}
		if dup := duplicateGrocery(items, it); dup != nil && !p.AllowDuplicates {
			skipped = append(skipped, SkippedItem{Name: it.Ingredient, Reason: "added to the list in the meantime", ExistingUID: dup.UID})
			continue
		}
		expect, send = append(expect, it), append(send, it)
	}
	if len(expect) == 0 {
		return &ToolError{Code: "duplicate", Message: "nothing was written: every proposed item is on the list already"}
	}
	if len(send) > 0 {
		if err := s.Client.AddGroceryItems(ctx, send); err != nil {
			return writeFailed(err)
		}
		if items, terr = s.pullGroceries(ctx); terr != nil {
			return notVerified(terr.Message)
		}
	}
	out.Items = make([]GroceryEntry, 0, len(expect))
	for _, want := range expect {
		got, present := findGrocery(items, want.UID)
		if !present {
			return notVerified("Paprika accepted the items but does not return " + want.Name + " yet")
		}
		if got.Name != want.Name || got.ListUID != want.ListUID {
			return &ToolError{Code: "uid_conflict", Message: "nothing was overwritten: Paprika already holds a different item under uid " + want.UID + "; propose again"}
		}
		out.Items = append(out.Items, groceryEntry(got))
	}
	out.List, out.Skipped = p.List, skipped
	out.Sync = s.Sync.Freshness(ctx, string(paprika.Groceries))
	return nil
}

func (s *server) pullMeals(ctx context.Context) ([]paprika.Meal, *ToolError) {
	return pull[paprika.Meal](ctx, s, paprika.Meals)
}

func (s *server) pullGroceries(ctx context.Context) ([]paprika.GroceryItem, *ToolError) {
	return pull[paprika.GroceryItem](ctx, s, paprika.Groceries)
}

// pull reads a collection straight from Paprika, bypassing cache freshness rules.
func pull[T any](ctx context.Context, s *server, col paprika.Collection) ([]T, *ToolError) {
	rows, err := s.Sync.Pull(ctx, col, nil)
	if err != nil {
		return nil, fromErr(err)
	}
	raws := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		raws = append(raws, r.JSON)
	}
	out, err := paprika.DecodeAll[T](raws)
	if err != nil {
		return nil, fromErr(err)
	}
	return out, nil
}

func findMeal(meals []paprika.Meal, uid string) (paprika.Meal, bool) {
	for _, m := range meals {
		if m.UID == uid {
			return m, true
		}
	}
	return paprika.Meal{}, false
}

func findGrocery(items []paprika.GroceryItem, uid string) (paprika.GroceryItem, bool) {
	for _, it := range items {
		if it.UID == uid {
			return it, true
		}
	}
	return paprika.GroceryItem{}, false
}

// writeFailed explains a failed POST. When the failure is a transport one the
// write may still have landed, and repeating the commit is the way to find out.
func writeFailed(err error) *ToolError {
	terr := fromErr(err)
	if terr.Retryable {
		terr.Message += "; it is unknown whether Paprika received the write. Call commit_proposal again: it checks Paprika first and cannot create duplicates"
	} else {
		terr.Message = "nothing was written: " + terr.Message
	}
	return terr
}

func notVerified(detail string) *ToolError {
	return &ToolError{Code: "write_not_verified", Retryable: true,
		Message: "the write was sent but could not be read back (" + detail + "). Call commit_proposal again to verify; it cannot create duplicates"}
}
