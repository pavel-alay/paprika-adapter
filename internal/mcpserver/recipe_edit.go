package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"paprika-adapter/internal/paprika"
)

const maxRecipeText = 20000

type ProposeRecipeEditIn struct {
	UID         string  `json:"uid" jsonschema:"recipe uid from search_recipes"`
	Rating      *int    `json:"rating,omitempty" jsonschema:"stars, 1 to 5; 0 removes the rating"`
	Notes       *string `json:"notes,omitempty" jsonschema:"REPLACES the recipe's notes; an empty string clears them. To add to the notes use append_note instead"`
	AppendNote  string  `json:"append_note,omitempty" jsonschema:"text added as a new paragraph after the existing notes; cannot be combined with notes"`
	Description *string `json:"description,omitempty" jsonschema:"REPLACES the recipe's description; an empty string clears it"`
	Directions  *string `json:"directions,omitempty" jsonschema:"REPLACES the recipe's directions in full, one step per line; cannot be empty"`
}

// recipeEdit is what a proposal stores. The note to append is kept apart from
// Edit.Notes because it is applied to the notes the recipe has at commit time.
type recipeEdit struct {
	UID        string             `json:"uid"`
	Name       string             `json:"name"`
	Edit       paprika.RecipeEdit `json:"edit"`
	AppendNote string             `json:"append_note,omitempty"`
	Proposed   []FieldChange      `json:"proposed"` // as shown to the user, with the values the recipe had then
}

type RecipeEditView struct {
	Recipe  NamedRef      `json:"recipe"`
	Changes []FieldChange `json:"changes" jsonschema:"one entry per field that changes"`
}

type FieldChange struct {
	Field string `json:"field" jsonschema:"rating, notes, description or directions"`
	From  string `json:"from" jsonschema:"value before the change; a rating is given as a number, 0 = unrated"`
	To    string `json:"to" jsonschema:"value after the change"`
}

func (s *server) proposeRecipeEdit(ctx context.Context, in ProposeRecipeEditIn, out *ProposeOut) *ToolError {
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		return invalid("uid is required")
	}
	edit := recipeEdit{UID: uid, Edit: paprika.RecipeEdit{Rating: in.Rating, Notes: cleanText(in.Notes), Description: cleanText(in.Description),
		Directions: cleanText(in.Directions)}, AppendNote: strings.TrimSpace(strings.ReplaceAll(in.AppendNote, "\r\n", "\n"))}
	if in.Rating != nil && (*in.Rating < 0 || *in.Rating > 5) {
		return invalid("rating must be 0 to 5")
	}
	if edit.Edit.Notes != nil && edit.AppendNote != "" {
		return invalid("give notes (replace) or append_note (add), not both")
	}
	if d := edit.Edit.Directions; d != nil && strings.TrimSpace(*d) == "" {
		return invalid("directions cannot be empty")
	}
	for field, v := range map[string]*string{"notes": edit.Edit.Notes, "append_note": &edit.AppendNote, "description": edit.Edit.Description, "directions": edit.Edit.Directions} {
		if v == nil {
			continue
		}
		if utf8.RuneCountInString(*v) > maxRecipeText {
			return invalid(fmt.Sprintf("%s is longer than %d characters", field, maxRecipeText))
		}
		if strings.ContainsRune(*v, 0) || !utf8.ValidString(*v) {
			return invalid(field + " is not valid text")
		}
	}
	if edit.empty() {
		return invalid("give at least one of rating, notes, append_note, description, directions")
	}
	if terr := s.ensureFresh(ctx, &out.Meta, paprika.Recipes); terr != nil {
		return terr
	}
	cached, ok, err := s.Store.Recipe(ctx, uid)
	if err != nil {
		return fromErr(err)
	}
	if !ok {
		return notFound("no recipe with uid " + uid)
	}
	recipe, err := paprika.Decode[paprika.Recipe](cached.Raw)
	if err != nil {
		return fromErr(err)
	}
	if recipe.InTrash {
		return invalid("recipe " + recipe.Name + " is in Paprika's trash")
	}
	edit.Name = recipe.Name
	changes := edit.changes(recipe)
	if len(changes) == 0 {
		return &ToolError{Code: "duplicate", Message: fmt.Sprintf("nothing to change: recipe %q already has these values", recipe.Name)}
	}
	var parts []string
	for _, c := range changes {
		switch {
		case c.Field == "rating":
			parts = append(parts, fmt.Sprintf("rating %s -> %s", stars(c.From), stars(c.To)))
		case c.Field == "notes" && edit.AppendNote != "":
			parts = append(parts, fmt.Sprintf("add to the notes: %s", preview(edit.AppendNote)))
		case c.To == "":
			parts = append(parts, fmt.Sprintf("clear the %s (%d characters)", c.Field, utf8.RuneCountInString(c.From)))
		default:
			parts = append(parts, fmt.Sprintf("replace the %s (%d characters) with: %s", c.Field, utf8.RuneCountInString(c.From), preview(c.To)))
		}
	}
	edit.Proposed = changes
	p := payload{Recipe: &edit, Summary: fmt.Sprintf("Change the Paprika recipe %q: %s. This overwrites what the recipe has now.", recipe.Name, strings.Join(parts, "; "))}
	view, terr := s.saveProposal(ctx, kindRecipe, p)
	if terr != nil {
		return terr
	}
	view.Recipe = &RecipeEditView{Recipe: NamedRef{UID: uid, Name: recipe.Name}, Changes: changes}
	out.Proposal = view
	return nil
}

// commitRecipeEdit reads the recipe from Paprika, overwrites the edited fields
// on that copy and reads it back. A change made elsewhere to the same field is
// overwritten without warning: that is what the user asked for.
func (s *server) commitRecipeEdit(ctx context.Context, p payload, out *CommitOut) *ToolError {
	edit := *p.Recipe
	current, err := s.Client.Recipe(ctx, edit.UID)
	if err != nil {
		return fromErr(err)
	}
	before, err := paprika.Decode[paprika.Recipe](current.JSON)
	if err != nil {
		return fromErr(err)
	}
	changes := edit.changes(before)
	after, stored := before, current
	if len(changes) == 0 { // an earlier attempt already landed; report what was proposed
		changes = append(changes, edit.Proposed...)
	} else {
		if _, err := s.Client.WriteRecipe(ctx, current, edit.resolved(before)); err != nil {
			return writeFailed(err)
		}
		if stored, err = s.Client.Recipe(ctx, edit.UID); err != nil {
			return notVerified(fromErr(err).Message)
		}
		if after, err = paprika.Decode[paprika.Recipe](stored.JSON); err != nil {
			return fromErr(err)
		}
		if left := edit.changes(after); len(left) > 0 {
			return notVerified("Paprika accepted the recipe but still returns the old " + left[0].Field)
		}
	}
	if err := s.Store.PutRecipe(ctx, stored, after.Hash, s.Now()); err != nil {
		return fromErr(err)
	}
	for i := range changes {
		changes[i].To = fieldValue(after, changes[i].Field)
	}
	out.Recipe = &RecipeEditView{Recipe: NamedRef{UID: after.UID, Name: after.Name}, Changes: changes}
	out.Sync = s.Sync.Freshness(ctx, paprika.Recipes)
	return nil
}

func (e recipeEdit) empty() bool {
	return e.Edit.Rating == nil && e.Edit.Notes == nil && e.Edit.Description == nil && e.Edit.Directions == nil && e.AppendNote == ""
}

// resolved turns the edit into plain field values for the recipe as it is now.
func (e recipeEdit) resolved(now paprika.Recipe) paprika.RecipeEdit {
	out := e.Edit
	if e.AppendNote == "" {
		return out
	}
	notes := strings.TrimRight(now.Notes, " \t\r\n")
	switch {
	case notes == "":
		notes = e.AppendNote
	case notes != e.AppendNote && !strings.HasSuffix(notes, "\n\n"+e.AppendNote): // not added yet
		notes += "\n\n" + e.AppendNote
	default:
		notes = now.Notes
	}
	out.Notes = &notes
	return out
}

// changes lists the fields whose value in now differs from what the edit wants.
func (e recipeEdit) changes(now paprika.Recipe) []FieldChange {
	want := e.resolved(now)
	var out []FieldChange
	if want.Rating != nil && *want.Rating != now.Rating {
		out = append(out, FieldChange{Field: "rating", From: fmt.Sprint(now.Rating), To: fmt.Sprint(*want.Rating)})
	}
	for _, f := range []struct {
		name string
		to   *string
	}{{"notes", want.Notes}, {"description", want.Description}, {"directions", want.Directions}} {
		if f.to != nil && *f.to != fieldValue(now, f.name) {
			out = append(out, FieldChange{Field: f.name, From: fieldValue(now, f.name), To: *f.to})
		}
	}
	return out
}

func fieldValue(r paprika.Recipe, field string) string {
	switch field {
	case "rating":
		return fmt.Sprint(r.Rating)
	case "notes":
		return r.Notes
	case "description":
		return r.Description
	default:
		return r.Directions
	}
}

func cleanText(v *string) *string {
	if v == nil {
		return nil
	}
	s := strings.ReplaceAll(*v, "\r\n", "\n")
	return &s
}

func stars(n string) string {
	if n == "0" {
		return "unrated"
	}
	return n + " of 5"
}

// preview shortens text for the one-line summary; the full text is in the proposal's changes.
func preview(text string) string {
	line := strings.Join(strings.Fields(text), " ")
	if r := []rune(line); len(r) > 160 {
		return fmt.Sprintf("%q... (%d characters in all)", string(r[:160]), utf8.RuneCountInString(text))
	}
	return fmt.Sprintf("%q", line)
}
