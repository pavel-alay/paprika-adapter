package paprika

import (
	"encoding/json"
	"fmt"
	"sort"
)

// TimeLayout is Paprika's wire timestamp. It carries no zone; values are passed
// through verbatim and never reinterpreted.
const TimeLayout = "2006-01-02 15:04:05"

// Collection is a v2 sync collection that is read with a single GET returning every row.
type Collection string

const (
	Categories         Collection = "categories"
	Pantry             Collection = "pantry"
	Meals              Collection = "meals"
	MealTypes          Collection = "mealtypes"
	GroceryLists       Collection = "grocerylists"
	Groceries          Collection = "groceries"
	GroceryAisles      Collection = "groceryaisles"
	GroceryIngredients Collection = "groceryingredients"
	Photos             Collection = "photos" // gallery photo metadata; the images are fetched on demand
)

// Recipes is not a Collection: it is listed as uid+hash refs and fetched one by one.
const Recipes = "recipes"

// Collections lists every replace-all collection the adapter reads, in sync order.
var Collections = []Collection{
	Categories, MealTypes, GroceryAisles, GroceryIngredients, GroceryLists, Groceries, Pantry, Meals, Photos,
}

// Raw is one entity exactly as Paprika returned it. The JSON is stored verbatim so
// fields this adapter does not know about survive in the cache and in exports.
type Raw struct {
	UID  string
	JSON json.RawMessage
}

// RecipeRef is one row of GET /sync/recipes/.
type RecipeRef struct {
	UID  string `json:"uid"`
	Hash string `json:"hash"`
}

// Recipe is the read projection of GET /sync/recipe/{uid}/.
type Recipe struct {
	UID             string   `json:"uid"`
	Name            string   `json:"name"`
	Ingredients     string   `json:"ingredients"`
	Directions      string   `json:"directions"`
	Description     string   `json:"description"`
	Notes           string   `json:"notes"`
	NutritionalInfo string   `json:"nutritional_info"`
	Servings        string   `json:"servings"`
	Difficulty      string   `json:"difficulty"`
	PrepTime        string   `json:"prep_time"`
	CookTime        string   `json:"cook_time"`
	TotalTime       string   `json:"total_time"`
	Source          string   `json:"source"`
	SourceURL       string   `json:"source_url"`
	Hash            string   `json:"hash"`
	Categories      []string `json:"categories"`
	Rating          int      `json:"rating"`
	InTrash         bool     `json:"in_trash"`
	OnFavorites     bool     `json:"on_favorites"`
	Created         string   `json:"created"`
	Photo           *string  `json:"photo"`       // file name of the cover thumbnail, nil when the recipe has no photo
	PhotoHash       *string  `json:"photo_hash"`  // SHA-256 of the thumbnail
	PhotoLarge      *string  `json:"photo_large"` // file name of the gallery photo that is the cover, if any
	PhotoURL        *string  `json:"photo_url"`   // signed link to the thumbnail, valid for minutes
}

// Photo is one gallery photo of a recipe as listed by GET /sync/photos/.
type Photo struct {
	UID       string `json:"uid"`
	RecipeUID string `json:"recipe_uid"`
	Filename  string `json:"filename"`
	Name      string `json:"name"`
	OrderFlag int    `json:"order_flag"`
	Hash      string `json:"hash"`
}

type Category struct {
	UID       string  `json:"uid"`
	Name      string  `json:"name"`
	ParentUID *string `json:"parent_uid"`
	OrderFlag int     `json:"order_flag"`
}

type PantryItem struct {
	UID            string  `json:"uid"`
	Ingredient     string  `json:"ingredient"`
	Quantity       string  `json:"quantity"`
	Aisle          string  `json:"aisle"`
	InStock        bool    `json:"in_stock"`
	HasExpiration  bool    `json:"has_expiration"`
	ExpirationDate *string `json:"expiration_date"`
	PurchaseDate   *string `json:"purchase_date"`
}

type Meal struct {
	UID       string  `json:"uid"`
	RecipeUID *string `json:"recipe_uid"`
	Name      string  `json:"name"`
	Date      string  `json:"date"`
	Type      int     `json:"type"`
	TypeUID   *string `json:"type_uid"`
	OrderFlag int     `json:"order_flag"`
}

type MealType struct {
	UID          string `json:"uid"`
	Name         string `json:"name"`
	OrderFlag    int    `json:"order_flag"`
	OriginalType *int   `json:"original_type"` // 0-3 for the built-in types, null for custom ones
}

type GroceryList struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	OrderFlag int    `json:"order_flag"`
	IsDefault bool   `json:"is_default"`
}

type GroceryItem struct {
	UID         string  `json:"uid"`
	ListUID     string  `json:"list_uid"`
	Name        string  `json:"name"`
	Ingredient  string  `json:"ingredient"`
	Quantity    string  `json:"quantity"`
	Instruction string  `json:"instruction"`
	Aisle       string  `json:"aisle"`
	AisleUID    *string `json:"aisle_uid"`
	OrderFlag   int     `json:"order_flag"`
	Purchased   bool    `json:"purchased"`
	Recipe      *string `json:"recipe"`
	RecipeUID   *string `json:"recipe_uid"`
}

type GroceryAisle struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	OrderFlag int    `json:"order_flag"`
}

type GroceryIngredient struct {
	UID      string  `json:"uid"`
	Name     string  `json:"name"`
	AisleUID *string `json:"aisle_uid"`
}

// Decode unmarshals one cached or fetched entity into its read projection.
func Decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("decode %T: %w", v, err)
	}
	return v, nil
}

// DecodeAll decodes every row, failing on the first one that does not fit.
func DecodeAll[T any](raws []json.RawMessage) ([]T, error) {
	out := make([]T, 0, len(raws))
	for _, r := range raws {
		v, err := Decode[T](r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// knownFields is the field set each endpoint returned when this client was written
// (Paprika 3 sync API v2). It exists only to detect drift, see Drift.
var knownFields = map[string][]string{
	Recipes: {
		"uid", "name", "ingredients", "directions", "description", "notes", "nutritional_info", "servings",
		"difficulty", "prep_time", "cook_time", "total_time", "source", "source_url", "image_url", "photo",
		"photo_hash", "photo_large", "scale", "hash", "categories", "rating", "in_trash", "is_pinned",
		"on_favorites", "on_grocery_list", "created", "photo_url",
		// Seen on a real account on 2026-10-09, null on every recipe there, so their value types are unknown.
		"prep_minutes", "cook_minutes", "total_minutes", "servings_min", "servings_max", "nutrition",
		"cookbook_uid", "metadata_version",
	},
	string(Categories): {"uid", "name", "parent_uid", "order_flag", "cookbook_uid"},
	string(Photos):     {"uid", "recipe_uid", "filename", "name", "order_flag", "hash"},
	string(Pantry): {
		"uid", "ingredient", "aisle", "expiration_date", "has_expiration", "in_stock", "purchase_date",
		"quantity", "aisle_uid", "location_uid", "notes",
	},
	string(Meals):     {"uid", "recipe_uid", "name", "date", "type", "type_uid", "order_flag", "is_ingredient", "scale"},
	string(MealTypes): {"uid", "name", "color", "order_flag", "original_type", "export_all_day", "export_time"},
	string(GroceryLists): {
		"uid", "name", "order_flag", "is_default", "reminders_list",
	},
	string(Groceries): {
		"uid", "recipe_uid", "name", "order_flag", "purchased", "aisle", "ingredient", "recipe", "instruction",
		"quantity", "separate", "aisle_uid", "list_uid",
	},
	string(GroceryAisles):      {"uid", "name", "order_flag"},
	string(GroceryIngredients): {"uid", "name", "aisle_uid"},
}

// Drift describes how the fields Paprika returned differ from the ones this client
// was built against. Either list being non-empty means the protocol has moved.
type Drift struct {
	Unknown []string `json:"unknown_fields,omitempty"` // returned by Paprika, not known to the adapter
	Missing []string `json:"missing_fields,omitempty"` // expected, absent from every returned row
}

func (d Drift) Empty() bool { return len(d.Unknown) == 0 && len(d.Missing) == 0 }

// DetectDrift compares the keys of rows against the known field set of endpoint.
// It inspects keys only, never values.
func DetectDrift(endpoint string, rows []Raw) Drift {
	known, ok := knownFields[endpoint]
	if !ok || len(rows) == 0 {
		return Drift{}
	}
	seen := map[string]bool{}
	for _, r := range rows {
		var obj map[string]json.RawMessage
		if json.Unmarshal(r.JSON, &obj) != nil {
			continue
		}
		for k := range obj {
			seen[k] = true
		}
	}
	var d Drift
	expected := map[string]bool{}
	for _, k := range known {
		expected[k] = true
		if !seen[k] {
			d.Missing = append(d.Missing, k)
		}
	}
	for k := range seen {
		if !expected[k] {
			d.Unknown = append(d.Unknown, k)
		}
	}
	sort.Strings(d.Unknown)
	sort.Strings(d.Missing)
	return d
}
