package paprika

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Paprika only accepts canonical 8-4-4-4-12 hex uids for entities a client creates.
var uidPattern = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)

// NewUID mints a uid in the format Paprika's own clients use.
func NewUID() string { return strings.ToUpper(uuid.NewString()) }

// NewMeal is one meal-plan entry to create. The wire shape mirrors what the
// Paprika 3 macOS client posts.
type NewMeal struct {
	UID       string  `json:"uid"`
	RecipeUID *string `json:"recipe_uid"` // nil for a free-text entry
	Name      string  `json:"name"`
	Date      string  `json:"date"` // "YYYY-MM-DD 00:00:00"
	Type      int     `json:"type"`
	TypeUID   string  `json:"type_uid"`
	OrderFlag int     `json:"order_flag"`
}

// NewGroceryItem is one grocery item to create.
type NewGroceryItem struct {
	UID         string  `json:"uid"`
	ListUID     string  `json:"list_uid"`
	Name        string  `json:"name"`
	Ingredient  string  `json:"ingredient"`
	Quantity    string  `json:"quantity"`
	Instruction string  `json:"instruction"`
	Aisle       string  `json:"aisle"`
	AisleUID    *string `json:"aisle_uid"`
	OrderFlag   int     `json:"order_flag"`
}

// NewPantryItem is one pantry item to create. Like the macOS client, the write
// carries no notes field.
type NewPantryItem struct {
	UID            string  `json:"uid"`
	Ingredient     string  `json:"ingredient"`
	Quantity       string  `json:"quantity"`
	Aisle          string  `json:"aisle"`
	AisleUID       string  `json:"aisle_uid"` // "" when no aisle is known, the way Paprika stores it
	InStock        bool    `json:"in_stock"`
	HasExpiration  bool    `json:"has_expiration"`
	ExpirationDate *string `json:"expiration_date"` // "YYYY-MM-DD 00:00:00"
	PurchaseDate   string  `json:"purchase_date"`   // "YYYY-MM-DD 00:00:00"
}

// MealEdit names what to overwrite on an existing meal-plan entry. Nil leaves a field as it is.
type MealEdit struct {
	Date    *string      `json:"date,omitempty"`    // "YYYY-MM-DD 00:00:00"
	Type    *MealSlot    `json:"type,omitempty"`    // which meal of the day
	Subject *MealSubject `json:"subject,omitempty"` // what is planned
}

// MealSlot is a meal type the way a meal row refers to it.
type MealSlot struct {
	UID  string `json:"uid"`
	Type int    `json:"type"` // the built-in number of the type, 0 for a custom one
}

// MealSubject is what a meal-plan entry plans: a recipe, or free text when RecipeUID is nil.
type MealSubject struct {
	RecipeUID *string `json:"recipe_uid"`
	Name      string  `json:"name"`
}

// EditMeal overwrites an existing meal-plan entry with current plus edit.
// Paprika replaces the whole row on write, so current must be the complete row
// as just read from Paprika; every other field in it is sent back unchanged.
func (c *Client) EditMeal(ctx context.Context, current Raw, edit MealEdit) error {
	obj, err := completeRow("edit meal", current)
	if err != nil {
		return err
	}
	if edit.Date != nil {
		obj["date"] = *edit.Date
	}
	if edit.Type != nil {
		obj["type_uid"], obj["type"] = edit.Type.UID, edit.Type.Type
	}
	if edit.Subject != nil {
		obj["recipe_uid"], obj["name"] = edit.Subject.RecipeUID, edit.Subject.Name
	}
	obj["deleted"] = false
	return c.upsert(ctx, Meals, []map[string]any{obj})
}

// PantryEdit names what to overwrite on an existing pantry item. Nil leaves a field as it is.
type PantryEdit struct {
	Ingredient   *string `json:"ingredient,omitempty"`
	Quantity     *string `json:"quantity,omitempty"`
	InStock      *bool   `json:"in_stock,omitempty"`
	Expiration   *string `json:"expiration,omitempty"`    // "YYYY-MM-DD 00:00:00"; "" removes the expiration date
	PurchaseDate *string `json:"purchase_date,omitempty"` // "YYYY-MM-DD 00:00:00"
}

// EditPantryItem overwrites an existing pantry item with current plus edit.
// As with EditMeal, current must be the complete row as just read from Paprika.
func (c *Client) EditPantryItem(ctx context.Context, current Raw, edit PantryEdit) error {
	obj, err := completeRow("edit pantry item", current)
	if err != nil {
		return err
	}
	if edit.Ingredient != nil {
		obj["ingredient"] = *edit.Ingredient
	}
	if edit.Quantity != nil {
		obj["quantity"] = *edit.Quantity
	}
	if edit.InStock != nil {
		obj["in_stock"] = *edit.InStock
	}
	if edit.Expiration != nil {
		obj["has_expiration"], obj["expiration_date"] = false, nil
		if *edit.Expiration != "" {
			obj["has_expiration"], obj["expiration_date"] = true, *edit.Expiration
		}
	}
	if edit.PurchaseDate != nil {
		obj["purchase_date"] = *edit.PurchaseDate
	}
	obj["deleted"] = false
	return c.upsert(ctx, Pantry, []map[string]any{obj})
}

// RemovePantryItems removes pantry items the way Paprika's own clients do: each
// complete row is posted again, marked deleted. Paprika then stops returning it.
func (c *Client) RemovePantryItems(ctx context.Context, current []Raw) error {
	const op = "remove pantry items"
	if len(current) == 0 {
		return &Error{Kind: KindInvalid, Op: op, Message: "no items"}
	}
	rows := make([]map[string]any, 0, len(current))
	for _, r := range current {
		obj, err := completeRow(op, r)
		if err != nil {
			return err
		}
		obj["deleted"] = true
		rows = append(rows, obj)
	}
	return c.upsert(ctx, Pantry, rows)
}

// completeRow decodes a row read from Paprika so that it can be posted back whole.
func completeRow(op string, current Raw) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(current.JSON, &obj); err != nil || current.UID == "" || obj["uid"] != current.UID {
		return nil, &Error{Kind: KindInvalid, Op: op, Message: "current row is not a complete Paprika row"}
	}
	return obj, nil
}

type mealWire struct {
	NewMeal
	Deleted bool `json:"deleted"`
}

type groceryWire struct {
	NewGroceryItem
	Recipe    *string `json:"recipe"`
	Purchased bool    `json:"purchased"`
	Separate  bool    `json:"separate"`
	Deleted   bool    `json:"deleted"`
}

// AddMeal creates one meal-plan entry. Paprika upserts by uid and replaces the
// whole object, so the caller must have checked that m.UID is not already in use.
func (c *Client) AddMeal(ctx context.Context, m NewMeal) error {
	if err := checkNewUID("add meal", m.UID); err != nil {
		return err
	}
	return c.upsert(ctx, Meals, []mealWire{{NewMeal: m}})
}

// AddGroceryItems creates grocery items in one request. The same uid caveat as
// AddMeal applies to every item.
func (c *Client) AddGroceryItems(ctx context.Context, items []NewGroceryItem) error {
	if len(items) == 0 {
		return &Error{Kind: KindInvalid, Op: "add grocery items", Message: "no items"}
	}
	wire := make([]groceryWire, 0, len(items))
	for _, it := range items {
		if err := checkNewUID("add grocery items", it.UID); err != nil {
			return err
		}
		wire = append(wire, groceryWire{NewGroceryItem: it})
	}
	return c.upsert(ctx, Groceries, wire)
}

type pantryWire struct {
	NewPantryItem
	Deleted bool `json:"deleted"`
}

// AddPantryItems creates pantry items in one request. The same uid caveat as
// AddMeal applies to every item.
func (c *Client) AddPantryItems(ctx context.Context, items []NewPantryItem) error {
	if len(items) == 0 {
		return &Error{Kind: KindInvalid, Op: "add pantry items", Message: "no items"}
	}
	wire := make([]pantryWire, 0, len(items))
	for _, it := range items {
		if err := checkNewUID("add pantry items", it.UID); err != nil {
			return err
		}
		wire = append(wire, pantryWire{NewPantryItem: it})
	}
	return c.upsert(ctx, Pantry, wire)
}

func checkNewUID(op, uid string) error {
	if !uidPattern.MatchString(uid) {
		return &Error{Kind: KindInvalid, Op: op, Message: "uid is not an uppercase canonical UUID"}
	}
	return nil
}

// upsert posts rows to a collection endpoint as a JSON array.
func (c *Client) upsert(ctx context.Context, col Collection, rows any) error {
	return c.postData(ctx, syncPath+string(col)+"/", "file", rows)
}

// postData posts payload the only way Paprika accepts a write: gzipped JSON in
// a multipart part named "data". It is sent once and never retried here.
func (c *Client) postData(ctx context.Context, path, filename string, payload any) error {
	op := "POST " + opPath(path)
	raw, err := json.Marshal(payload)
	if err != nil {
		return &Error{Kind: KindInvalid, Op: op, Message: "rows are not serialisable"}
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw)
	if err := zw.Close(); err != nil {
		return &Error{Kind: KindInvalid, Op: op, Message: "cannot compress rows"}
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	part, err := mw.CreateFormFile("data", filename)
	if err == nil {
		_, err = part.Write(gz.Bytes())
	}
	if cerr := mw.Close(); err != nil || cerr != nil {
		return &Error{Kind: KindInvalid, Op: op, Message: "cannot build multipart body"}
	}
	res, err := c.call(ctx, http.MethodPost, path, &body{contentType: mw.FormDataContentType(), data: form.Bytes()})
	if err != nil {
		return err
	}
	if string(bytes.TrimSpace(res)) != "true" {
		return &Error{Kind: KindProtocol, Op: op, Status: http.StatusOK, Message: "write was not acknowledged with result:true"}
	}
	return nil
}
