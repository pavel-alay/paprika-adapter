package mcpserver

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// StatusTool is always served: it is how an operator or agent sees that the adapter is healthy.
	StatusTool = "sync_status"
	// CommitTool is served whenever at least one propose tool is, since a proposal is useless without it.
	CommitTool = "commit_proposal"
)

// ReadTools are the tools that only read the cache. StatusTool is among them.
var ReadTools = []string{
	"search_recipes", "get_recipe", "get_recipe_photo", "list_pantry", "get_meal_plan",
	"list_grocery_lists", "list_grocery_items", StatusTool,
}

// ProposeTools are the write tools that prepare a proposal. They are served only when writes are enabled.
var ProposeTools = []string{
	"propose_meal_plan_entry", "propose_meal_plan_edit", "propose_grocery_items", "propose_pantry_items",
	"propose_pantry_edit", "propose_pantry_removal", "propose_recipe_edit",
}

// allTools lists every tool the server can serve.
func allTools() []string {
	return slices.Concat(ReadTools, ProposeTools, []string{CommitTool})
}

// HasWriteTools reports whether an allowlist names a propose tool or CommitTool.
func HasWriteTools(list []string) bool {
	return slices.ContainsFunc(list, func(n string) bool { return n == CommitTool || slices.Contains(ProposeTools, n) })
}

// ParseTools reads an allowlist of tool names separated by commas or whitespace.
// An empty list means every tool. A name the server does not know is an error,
// so that a typo cannot silently leave a tool switched on or off.
func ParseTools(raw string) ([]string, error) {
	names := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	known := allTools()
	var unknown []string
	for _, n := range names {
		if !slices.Contains(known, n) {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(known)
		return nil, fmt.Errorf("unknown tool(s) %s; known tools: %s", strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	if len(names) == 0 {
		return nil, nil
	}
	if slices.Contains(names, CommitTool) && !slices.ContainsFunc(names, func(n string) bool { return slices.Contains(ProposeTools, n) }) {
		return nil, fmt.Errorf("%s is listed without any propose tool, so there would be nothing to commit", CommitTool)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// allowed returns the tools to serve for an allowlist, adding the ones that are
// implied: StatusTool always, CommitTool when any propose tool is listed.
// A nil result means every tool.
func allowed(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	set := map[string]bool{StatusTool: true}
	for _, n := range list {
		set[n] = true
		if slices.Contains(ProposeTools, n) {
			set[CommitTool] = true
		}
	}
	return set
}

// proposeToolOf names the propose tool that creates each kind of proposal.
var proposeToolOf = map[string]string{
	kindMeal:          "propose_meal_plan_entry",
	kindMealEdit:      "propose_meal_plan_edit",
	kindGroceries:     "propose_grocery_items",
	kindPantry:        "propose_pantry_items",
	kindPantryEdit:    "propose_pantry_edit",
	kindPantryRemoval: "propose_pantry_removal",
	kindRecipe:        "propose_recipe_edit",
}

// commitAllowed says whether the allowlist lets a proposal of this kind be committed:
// only when the propose tool that creates it is served, so that commit_proposal
// cannot write a kind of change the operator left out.
func commitAllowed(list []string, kind string) bool {
	set := allowed(list)
	return set == nil || set[proposeToolOf[kind]]
}

// registry registers only the tools an allowlist lets through.
type registry struct {
	srv   *mcp.Server
	allow map[string]bool // nil = every tool
}

func (r registry) serves(name string) bool { return r.allow == nil || r.allow[name] }
