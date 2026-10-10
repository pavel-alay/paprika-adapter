// Package mcpserver exposes the cache and the two guarded writes as MCP tools.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
	"paprika-adapter/internal/syncer"
)

// Deps is everything the tools need.
type Deps struct {
	Store         *store.Store
	Sync          *syncer.Syncer
	Client        *paprika.Client
	WritesEnabled bool
	Location      *time.Location // decides what "today" and "this week" mean
	Now           func() time.Time
	Version       string
	ProposalTTL   time.Duration
	// RequireHumanApproval refuses a commit unless the user approved it through
	// MCP elicitation. When false, a client without elicitation commits on confirm=true alone.
	RequireHumanApproval bool
}

type server struct {
	Deps
	writeMu sync.Mutex // one commit at a time
}

// New builds the MCP server. Write tools are registered only when writes are enabled.
func New(d Deps) *mcp.Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Location == nil {
		d.Location = time.Local
	}
	if d.ProposalTTL <= 0 {
		d.ProposalTTL = 15 * time.Minute
	}
	s := &server{Deps: d}
	srv := mcp.NewServer(&mcp.Implementation{Name: "paprika-adapter", Title: "Paprika household adapter", Version: d.Version}, &mcp.ServerOptions{
		Instructions: "Read-only view of one household's Paprika 3 account (recipes, pantry, meal plan, grocery lists) " +
			"served from a local cache. Every result carries a sync block: treat state=stale as possibly outdated and say so. " +
			"Writes, when enabled, are two-step: propose, show the summary to the user, and call commit_proposal only after they approve. " +
			"Writes add rows, except the edit tools (propose_recipe_edit, propose_meal_plan_edit, propose_pantry_edit), which overwrite an existing " +
			"recipe, meal-plan entry or pantry item, and propose_pantry_removal, which removes pantry items. " +
			"If the client supports MCP elicitation the user is also asked directly before anything is written.",
	})

	add(srv, readTool("search_recipes", "Search recipes",
		"Full-text search over cached Paprika recipes. Returns compact summaries; use get_recipe for ingredients and directions."), s.searchRecipes)
	add(srv, readTool("get_recipe", "Get recipe",
		"Full details of one recipe by uid, including Paprika's raw ingredient text and a best-effort parsed version of it."), s.getRecipe)
	add(srv, readTool("get_recipe_photo", "Get recipe photo",
		"A recipe's photo as an image. By default the small cover thumbnail; size=full returns a full-resolution gallery photo, "+
			"which only some recipes have. get_recipe says whether a recipe has a photo and how many gallery photos."), s.getRecipePhoto)
	add(srv, readTool("list_pantry", "List pantry",
		"Pantry items from Paprika, optionally filtered by ingredient text or stock status."), s.listPantry)
	add(srv, readTool("get_meal_plan", "Get meal plan",
		"Dated meal-plan entries for a date range (default: the current Monday-to-Sunday week), plus the meal types available for planning."), s.getMealPlan)
	add(srv, readTool("list_grocery_lists", "List grocery lists",
		"Paprika grocery lists with item counts. Use a list's uid with list_grocery_items or propose_grocery_items."), s.listGroceryLists)
	add(srv, readTool("list_grocery_items", "List grocery items",
		"Items on one grocery list. Purchased items are left out unless include_purchased is true."), s.listGroceryItems)
	add(srv, readTool("sync_status", "Sync status",
		"Health of the adapter: per-collection freshness, last sync errors, protocol drift, and whether writes are enabled."), s.syncStatus)

	if d.WritesEnabled {
		add(srv, writeTool("propose_meal_plan_entry", "Propose meal plan entry",
			"Prepare ONE meal-plan entry for a date. Writes nothing. Returns a proposal_id and a summary to show the user; "+
				"the entry is created only by commit_proposal.", false, false), s.proposeMeal)
		add(srv, writeTool("propose_meal_plan_edit", "Propose meal plan edit",
			"Prepare a change to ONE existing meal-plan entry: move it to another day, change its meal type, or plan a different "+
				"recipe or text in its place. Writes nothing. Returns a proposal_id, a summary, and the entry before and after.", false, false), s.proposeMealEdit)
		add(srv, writeTool("propose_grocery_items", "Propose grocery items",
			"Prepare items to add to one grocery list. Writes nothing. Items already on the list (unpurchased, same name) are skipped "+
				"unless allow_duplicates is true. Returns a proposal_id and a summary to show the user.", false, false), s.proposeGroceries)
		add(srv, writeTool("propose_pantry_items", "Propose pantry items",
			"Prepare items to add to the Paprika pantry, as in stock and bought today. Writes nothing. Ingredients already in the pantry "+
				"are skipped unless allow_duplicates is true. Returns a proposal_id and a summary to show the user.", false, false), s.proposePantry)
		add(srv, writeTool("propose_pantry_edit", "Propose pantry edit",
			"Prepare a change to ONE existing pantry item: its quantity, whether it is in stock, its expiration or purchase date, "+
				"or its name. Writes nothing. Returns a proposal_id, a summary, and the item before and after.", false, false), s.proposePantryEdit)
		add(srv, writeTool("propose_pantry_removal", "Propose pantry removal",
			"Prepare the removal of pantry items, named by uid. Writes nothing. To record that something is used up but keep it "+
				"in the pantry, use propose_pantry_edit with in_stock=false instead. Returns a proposal_id, a summary and the items "+
				"that would be removed.", false, false), s.proposePantryRemoval)
		add(srv, writeTool("propose_recipe_edit", "Propose recipe edit",
			"Prepare a change to an existing recipe: its star rating, notes, description or directions. Writes nothing. "+
				"A given field REPLACES what the recipe has; append_note adds to the notes instead. Returns a proposal_id, a summary "+
				"and the old and new value of every field to show the user.", false, false), s.proposeRecipeEdit)
		add(srv, writeTool("commit_proposal", "Commit proposal",
			"Write a proposal to Paprika after the user approved it, then read it back to verify. If the client supports "+
				"MCP elicitation the user is asked to approve the exact summary first. Safe to repeat: a retried commit never creates duplicates. "+
				"Committing an edit overwrites the edited fields of that recipe, meal-plan entry or pantry item; committing a removal "+
				"removes the named pantry items.", true, true), s.commitProposal)
	}
	return srv
}

func readTool(name, title, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Title: title, Description: description,
		Annotations: &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}}
}

// writeTool describes a tool of the write path. overwrites is true for one that
// can replace existing content, as opposed to only adding rows.
func writeTool(name, title, description string, touchesPaprika, overwrites bool) *mcp.Tool {
	return &mcp.Tool{Name: name, Title: title, Description: description,
		Annotations: &mcp.ToolAnnotations{Title: title, IdempotentHint: true, DestructiveHint: &overwrites, OpenWorldHint: &touchesPaprika}}
}

// Meta is the common head of every tool result.
type Meta struct {
	OK    bool               `json:"ok" jsonschema:"false when the call failed; error says why"`
	Error *ToolError         `json:"error,omitempty"`
	Sync  []syncer.Freshness `json:"sync,omitzero" jsonschema:"freshness of each Paprika collection this result was built from"`
}

func (m *Meta) meta() *Meta { return m }

// ToolError is a failure the agent can act on.
type ToolError struct {
	Code      string `json:"code" jsonschema:"stable machine-readable code, e.g. invalid_input, not_found, auth_failed, upstream_unavailable, protocol_changed, duplicate, confirmation_required, approval_declined, approval_unavailable, proposal_expired, write_not_verified, uid_conflict"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable" jsonschema:"true when repeating the same call later may succeed without anyone changing anything"`
}

func invalid(message string) *ToolError  { return &ToolError{Code: "invalid_input", Message: message} }
func notFound(message string) *ToolError { return &ToolError{Code: "not_found", Message: message} }

// fromErr maps any error to a ToolError without leaking internals of a Paprika exchange.
func fromErr(err error) *ToolError {
	var pe *paprika.Error
	if errors.As(err, &pe) {
		return &ToolError{Code: string(pe.Kind), Message: pe.Message, Retryable: pe.Retryable()}
	}
	return &ToolError{Code: "cache_error", Message: err.Error()}
}

type envelope interface{ meta() *Meta }

// call gives a handler access to the request it is serving, and lets it put a
// question to the client (ask) instead of finishing.
type call struct {
	req     *mcp.CallToolRequest
	ask     mcp.InputRequestMap
	content []mcp.Content // extra blocks for the reply, such as an image
}

type callKey struct{}

// add registers a typed tool whose handler fills out and returns nil, or returns a ToolError.
func add[In, Out any, P interface {
	*Out
	envelope
}](srv *mcp.Server, tool *mcp.Tool, h func(context.Context, In, P) *ToolError) {
	mcp.AddTool(srv, tool, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var out Out
		c := &call{req: req}
		terr := h(context.WithValue(ctx, callKey{}, c), in, &out)
		if c.ask != nil {
			// The SDK puts the question to the client and runs the handler again with the answer.
			return &mcp.CallToolResult{InputRequests: c.ask}, out, nil
		}
		m := P(&out).meta()
		m.OK, m.Error = terr == nil, terr
		res := &mcp.CallToolResult{IsError: terr != nil}
		if terr == nil && c.content != nil {
			// With content of its own the SDK adds no text copy of the result, so add it here.
			text, err := json.Marshal(out)
			if err != nil {
				return nil, out, err
			}
			res.Content = append([]mcp.Content{&mcp.TextContent{Text: string(text)}}, c.content...)
		}
		return res, out, nil
	})
}

// ensure refreshes the collections a read needs and refuses to answer from a
// cache that has never been filled.
func (s *server) ensure(ctx context.Context, m *Meta, collections ...string) *ToolError {
	m.Sync = s.Sync.Ensure(ctx, collections...)
	for _, f := range m.Sync {
		if f.State != syncer.StateEmpty {
			continue
		}
		if f.Error != nil {
			return &ToolError{Code: f.Error.Code, Message: "no cached " + f.Collection + " yet: " + f.Error.Message,
				Retryable: f.Error.Code == string(paprika.KindUnavailable)}
		}
		return &ToolError{Code: string(paprika.KindUnavailable), Retryable: true,
			Message: "no cached " + f.Collection + " yet; the first sync is still running, try again shortly"}
	}
	return nil
}

// NamedRef points at a Paprika entity by uid with its display name.
type NamedRef struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

// dateOnly reduces Paprika's "YYYY-MM-DD 00:00:00" to the calendar date it means.
func dateOnly(v string) string {
	if len(v) >= 10 {
		return v[:10]
	}
	return v
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
