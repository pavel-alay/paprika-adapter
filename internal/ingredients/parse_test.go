package ingredients

import "testing"

func TestParse(t *testing.T) {
	for raw, want := range map[string]Line{
		"2 cups all-purpose flour":     {Quantity: "2", Unit: "cups", Item: "all-purpose flour"},
		"1 1/2 tsp kosher salt":        {Quantity: "1 1/2", Unit: "tsp", Item: "kosher salt"},
		"½ tsp black pepper":           {Quantity: "½", Unit: "tsp", Item: "black pepper"},
		"1/4 tsp instant yeast":        {Quantity: "1/4", Unit: "tsp", Item: "instant yeast"},
		"2-3 cloves garlic, minced":    {Quantity: "2-3", Unit: "cloves", Item: "garlic", Note: "minced"},
		"1.5 kg potatoes":              {Quantity: "1.5", Unit: "kg", Item: "potatoes"},
		"2 eggs":                       {Quantity: "2", Item: "eggs"},
		"1 whole chicken (about 4 lb)": {Quantity: "1", Item: "whole chicken (about 4 lb)"},
		"salt to taste":                {Item: "salt to taste"},
		"  3 Tbsp. butter, melted  ":   {Quantity: "3", Unit: "Tbsp.", Item: "butter", Note: "melted"},
		"For the sauce:":               {Heading: true},
		"1 lemon, juiced, plus wedges": {Quantity: "1", Item: "lemon", Note: "juiced, plus wedges"},
		"100 g":                        {Quantity: "100", Unit: "g"},
		"Zest of 1 orange":             {Item: "Zest of 1 orange"},
	} {
		got := Parse(raw)
		if len(got) != 1 {
			t.Fatalf("%q: %d lines", raw, len(got))
		}
		want.Raw = raw
		if got[0] != want {
			t.Errorf("%q:\n got %+v\nwant %+v", raw, got[0], want)
		}
	}
}

func TestParseKeepsEveryNonBlankLineVerbatim(t *testing.T) {
	text := "1 cup rice\r\n\n   \n\tpinch of salt\nGarnish:\n"
	got := Parse(text)
	if len(got) != 3 || got[0].Raw != "1 cup rice" || got[1].Raw != "\tpinch of salt" || !got[2].Heading {
		t.Fatalf("lines = %+v", got)
	}
	if lines := Parse(""); lines == nil || len(lines) != 0 {
		t.Fatalf("empty text must give an empty list, got %#v", lines)
	}
}
