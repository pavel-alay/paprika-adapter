package paprika

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// RecipeEdit names the recipe fields a caller may overwrite. Nil leaves a field as it is.
type RecipeEdit struct {
	Rating      *int    `json:"rating,omitempty"`
	Notes       *string `json:"notes,omitempty"`
	Description *string `json:"description,omitempty"`
	Directions  *string `json:"directions,omitempty"`
}

// Fields Paprika returns on read but its own client leaves out of a recipe write.
var recipeReadOnly = []string{"photo_url", "on_grocery_list"}

// WriteRecipe overwrites an existing recipe with current plus edit. Paprika
// replaces the whole recipe on write, so current must be the complete object
// as just read from Paprika: every field in it, known to this adapter or not,
// is sent back unchanged apart from the edited ones and the content hash.
// It returns the object that was sent.
func (c *Client) WriteRecipe(ctx context.Context, current Raw, edit RecipeEdit) (json.RawMessage, error) {
	const op = "write recipe"
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(current.JSON, &obj); err != nil || current.UID == "" {
		return nil, &Error{Kind: KindInvalid, Op: op, Message: "current recipe is not a complete Paprika recipe object"}
	}
	for field, v := range map[string]any{"rating": edit.Rating, "notes": edit.Notes, "description": edit.Description, "directions": edit.Directions} {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, &Error{Kind: KindInvalid, Op: op, Message: field + " is not serialisable"}
		}
		if string(raw) != "null" {
			obj[field] = raw
		}
	}
	for _, k := range recipeReadOnly {
		delete(obj, k)
	}
	obj["deleted"] = json.RawMessage("false")
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, &Error{Kind: KindInvalid, Op: op, Message: "recipe is not serialisable"}
	}
	hash, err := RecipeHash(body)
	if err != nil {
		return nil, err
	}
	obj["hash"], _ = json.Marshal(hash)
	if body, err = json.Marshal(obj); err != nil {
		return nil, &Error{Kind: KindInvalid, Op: op, Message: "recipe is not serialisable"}
	}
	// A recipe is posted as one object to its own URL, not as an array to a collection URL.
	if err := c.postData(ctx, syncPath+"recipe/"+url.PathEscape(current.UID)+"/", "data.gz", json.RawMessage(body)); err != nil {
		return nil, err
	}
	return body, nil
}

// hashedFields are the recipe fields that make up Paprika's content hash, in
// the order they are hashed (alphabetical by key), each with how its value is
// taken. The list and the rules come from the notes of bojanrajkovic/mcp-paprika,
// which derived them from Paprika's own framework.
var hashedFields = []struct {
	key  string
	rule byte
}{
	{"categories", 'c'}, {"cook_time", 't'}, {"created", '='}, {"deleted", 'f'}, {"description", 't'},
	{"difficulty", 't'}, {"directions", 't'}, {"hash", '0'}, {"image_url", '='}, {"in_trash", 'f'},
	{"ingredients", 't'}, {"is_pinned", '='}, {"name", '='}, {"notes", 't'}, {"nutritional_info", 't'},
	{"on_favorites", '='}, {"photo", '='}, {"photo_hash", '='}, {"photo_large", '='}, {"prep_time", 't'},
	{"rating", '='}, {"scale", '='}, {"servings", 't'}, {"source", 't'}, {"source_url", 't'},
	{"total_time", 't'}, {"uid", '='},
}

// RecipeHash computes the content hash Paprika's apps stamp on a recipe: the
// uppercase SHA-256 of a compact JSON array of the hashed fields. Paprika does
// not check it on an ordinary save; other devices use it to notice a change.
//
// Rules: 't' text, null counts as ""; '=' the stored value as it is; 'c' the
// category uids sorted bytewise; 'f' always false, so trashing does not change
// the hash; '0' always null, the hash does not cover itself.
func RecipeHash(recipe json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(recipe))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return "", &Error{Kind: KindInvalid, Op: "hash recipe", Message: "recipe is not a JSON object"}
	}
	values := make([]any, 0, len(hashedFields))
	for _, f := range hashedFields {
		v := obj[f.key]
		switch f.rule {
		case 't':
			if v == nil {
				v = ""
			}
		case 'f':
			v = false
		case '0':
			v = nil
		case 'c':
			uids := []any{}
			list, _ := v.([]any)
			for _, u := range list {
				uids = append(uids, u)
			}
			sort.Slice(uids, func(i, j int) bool { return fmt.Sprint(uids[i]) < fmt.Sprint(uids[j]) })
			v = uids
		}
		values = append(values, v)
	}
	var b strings.Builder
	if err := writeCanonical(&b, values); err != nil {
		return "", err
	}
	return fmt.Sprintf("%X", sha256.Sum256([]byte(b.String()))), nil
}

// writeCanonical serialises v the way Apple's JSON serialiser does by default:
// compact, non-ASCII left as UTF-8, and "/" escaped.
func writeCanonical(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		fmt.Fprint(b, x)
	case json.Number:
		b.WriteString(x.String())
	case string:
		b.WriteByte('"')
		for _, r := range x {
			switch r {
			case '"', '\\', '/':
				b.WriteByte('\\')
				b.WriteRune(r)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			default:
				if r < 0x20 {
					fmt.Fprintf(b, `\u%04x`, r)
				} else {
					b.WriteRune(r)
				}
			}
		}
		b.WriteByte('"')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		return &Error{Kind: KindInvalid, Op: "hash recipe", Message: "recipe holds a value of an unexpected type in a hashed field"}
	}
	return nil
}
