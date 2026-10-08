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

func checkNewUID(op, uid string) error {
	if !uidPattern.MatchString(uid) {
		return &Error{Kind: KindInvalid, Op: op, Message: "uid is not an uppercase canonical UUID"}
	}
	return nil
}

// upsert posts rows to a collection endpoint as a gzipped JSON array in a
// multipart part named "data". It is sent once and never retried here.
func (c *Client) upsert(ctx context.Context, col Collection, rows any) error {
	path := syncPath + string(col) + "/"
	op := "POST " + path
	payload, err := json.Marshal(rows)
	if err != nil {
		return &Error{Kind: KindInvalid, Op: op, Message: "rows are not serialisable"}
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(payload)
	if err := zw.Close(); err != nil {
		return &Error{Kind: KindInvalid, Op: op, Message: "cannot compress rows"}
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	part, err := mw.CreateFormFile("data", "file")
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
