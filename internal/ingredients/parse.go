// Package ingredients offers a best-effort reading of Paprika's free-text
// ingredient lines. The raw text is authoritative; a parse is a convenience.
package ingredients

import (
	"regexp"
	"strings"
)

// Line is one ingredient line. Raw is always the untouched text from Paprika.
type Line struct {
	Raw      string `json:"raw" jsonschema:"the line exactly as stored in Paprika"`
	Quantity string `json:"quantity,omitempty" jsonschema:"leading amount as written, e.g. 1 1/2"`
	Unit     string `json:"unit,omitempty" jsonschema:"unit as written, when recognised"`
	Item     string `json:"item,omitempty" jsonschema:"what remains after quantity and unit, up to the first comma"`
	Note     string `json:"note,omitempty" jsonschema:"preparation note after the first comma"`
	Heading  bool   `json:"heading,omitempty" jsonschema:"true for a section heading such as For the sauce:"`
}

var (
	quantity = regexp.MustCompile(`^(\d+\s+\d+/\d+|\d+/\d+|\d+(?:[.,]\d+)?\s*[¼½¾⅓⅔⅛⅜⅝⅞]?|[¼½¾⅓⅔⅛⅜⅝⅞])(?:\s*(?:-|–|to)\s*(\d+/\d+|\d+(?:[.,]\d+)?))?\s*`)
	units    = map[string]bool{}
)

func init() {
	for _, u := range strings.Fields(`tsp tsps teaspoon teaspoons tbsp tbsps tablespoon tablespoons cup cups oz ounce ounces
		lb lbs pound pounds g gram grams kg ml l liter liters litre litres qt quart quarts pt pint pints gal gallon
		clove cloves can cans head heads bunch bunches pinch pinches stick sticks slice slices piece pieces
		package packages pkg jar jars bottle bottles sprig sprigs dash`) {
		units[u] = true
	}
}

// Parse splits ingredient text into lines. Blank lines are dropped; every other
// line is returned with Raw set, parsed or not.
func Parse(text string) []Line {
	out := []Line{}
	for raw := range strings.SplitSeq(text, "\n") {
		raw = strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		line := Line{Raw: raw}
		if strings.HasSuffix(trimmed, ":") {
			line.Heading = true
			out = append(out, line)
			continue
		}
		rest := trimmed
		if m := quantity.FindString(rest); m != "" {
			line.Quantity = strings.TrimSpace(m)
			rest = rest[len(m):]
			if word, after, _ := strings.Cut(rest, " "); units[strings.ToLower(strings.TrimSuffix(word, "."))] {
				line.Unit, rest = word, after
			}
		}
		item, note, _ := strings.Cut(rest, ",")
		line.Item, line.Note = strings.TrimSpace(item), strings.TrimSpace(note)
		out = append(out, line)
	}
	return out
}
