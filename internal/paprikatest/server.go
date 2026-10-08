// Package paprikatest is an in-memory stand-in for the Paprika sync API, seeded
// from synthetic fixtures. It reproduces the behaviours the adapter depends on:
// form login, bearer auth, the result/error envelope (including errors sent as
// HTTP 200), change counters, and gzipped-multipart upserts keyed by uid.
package paprikatest

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed fixtures/*.json
var fixtures embed.FS

const (
	Email    = "household@example.test"
	Password = "fixture-password-do-not-log"
)

var (
	collections = []string{
		"categories", "pantry", "meals", "mealtypes", "grocerylists", "groceries", "groceryaisles", "groceryingredients",
	}
	writable  = map[string]bool{"meals": true, "groceries": true}
	uidShape  = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	dayMarker = regexp.MustCompile(`@([DP])(\d+)`)
)

type row = map[string]any

// Server is a fake Paprika backend. All methods are safe for concurrent use.
// By default it is reached in-process through Client, with no socket involved.
type Server struct {
	URL string

	listener *httptest.Server

	mu       sync.Mutex
	tokenSeq int
	token    string
	recipes  []row
	rows     map[string][]row
	counters map[string]int64
	requests []string
	failures map[string][]int // "METHOD path" -> queued HTTP statuses
	dropAck  int              // writes to apply and then answer with 502
	down     bool             // answer everything with 503
}

// New starts a fake seeded with the fixtures. Meal dates are laid out around the
// week (Monday to Sunday) containing now.
func New(now time.Time) *Server {
	s := &Server{rows: map[string][]row{}, counters: map[string]int64{}, failures: map[string][]int{}}
	monday := now.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
	s.recipes = load("recipes", monday)
	for _, c := range collections {
		s.rows[c] = load(c, monday)
		s.counters[c] = 1
	}
	s.counters["recipes"] = 1
	s.URL = "https://paprika.test"
	return s
}

// Client returns an HTTP client whose requests are answered by this fake in-process.
func (s *Server) Client() *http.Client { return HandlerClient(http.HandlerFunc(s.serve)) }

// HandlerClient returns an HTTP client that hands every request straight to h.
func HandlerClient(h http.Handler) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Result(), nil
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Listen additionally serves the fake on a loopback port, for a client in
// another process, and sets URL to it.
func (s *Server) Listen() {
	s.listener = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.listener.URL
}

// Close stops the listener started by Listen, if any.
func (s *Server) Close() {
	if s.listener != nil {
		s.listener.Close()
	}
}

func load(name string, monday time.Time) []row {
	raw, err := fixtures.ReadFile("fixtures/" + name + ".json")
	if err != nil {
		panic(err)
	}
	text := dayMarker.ReplaceAllStringFunc(string(raw), func(m string) string {
		n, _ := strconv.Atoi(m[2:])
		if m[1] == 'P' {
			n = -n
		}
		return monday.AddDate(0, 0, n).Format("2006-01-02")
	})
	var rows []row
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		panic(fmt.Sprintf("fixture %s: %v", name, err))
	}
	return rows
}

// Hits counts requests whose "METHOD path" starts with prefix.
func (s *Server) Hits(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// FailNext makes the next len(statuses) requests to "METHOD path" fail with the
// given HTTP statuses, without touching any data.
func (s *Server) FailNext(methodAndPath string, statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[methodAndPath] = append(s.failures[methodAndPath], statuses...)
}

// DropNextWriteAck applies the next write but answers it with a 502, the way a
// lost response looks to the client.
func (s *Server) DropNextWriteAck() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropAck++
}

// SetDown makes every request fail with 503 until it is switched off again.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// RevokeToken invalidates the token every client currently holds.
func (s *Server) RevokeToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
}

// Rows returns a copy of the live (not deleted) rows of a collection.
func (s *Server) Rows(collection string) []row {
	s.mu.Lock()
	defer s.mu.Unlock()
	return live(s.rows[collection])
}

// UpdateRecipe changes one recipe field and gives the recipe a new hash, as an
// edit in a Paprika app would.
func (s *Server) UpdateRecipe(uid, field string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recipes {
		if r["uid"] == uid {
			r[field] = value
			r["hash"] = fmt.Sprintf("%064X", s.counters["recipes"]+1)
			s.counters["recipes"]++
			return
		}
	}
	panic("unknown recipe " + uid)
}

// RemoveRecipe deletes a recipe outright.
func (s *Server) RemoveRecipe(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.recipes[:0]
	for _, r := range s.recipes {
		if r["uid"] != uid {
			kept = append(kept, r)
		}
	}
	s.recipes = kept
	s.counters["recipes"]++
}

// Put inserts or replaces a row directly, standing in for an edit made in a Paprika app.
func (s *Server) Put(collection string, r row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsert(collection, r)
}

func (s *Server) upsert(collection string, r row) {
	s.counters[collection]++
	for i, old := range s.rows[collection] {
		if old["uid"] == r["uid"] {
			s.rows[collection][i] = r
			return
		}
	}
	s.rows[collection] = append(s.rows[collection], r)
}

func live(rows []row) []row {
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		if r["deleted"] == true {
			continue
		}
		c := row{}
		for k, v := range r {
			if k != "deleted" {
				c[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	s.requests = append(s.requests, key)
	if s.down {
		fail(w, http.StatusServiceUnavailable, "Service Unavailable")
		return
	}
	if q := s.failures[key]; len(q) > 0 {
		s.failures[key] = q[1:]
		fail(w, q[0], http.StatusText(q[0]))
		return
	}
	if r.URL.Path == "/api/v1/account/login/" && r.Method == http.MethodPost {
		s.login(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v2/sync/")
	if !ok {
		fail(w, http.StatusNotFound, "Not found.")
		return
	}
	if s.token == "" || r.Header.Get("Authorization") != "Bearer "+s.token {
		fail(w, http.StatusUnauthorized, "Invalid or expired credentials.")
		return
	}
	name := strings.TrimSuffix(rest, "/")
	switch {
	case r.Method == http.MethodGet && name == "status":
		result(w, s.counters)
	case r.Method == http.MethodGet && name == "recipes":
		refs := make([]row, 0, len(s.recipes))
		for _, rec := range s.recipes {
			refs = append(refs, row{"uid": rec["uid"], "hash": rec["hash"]})
		}
		result(w, refs)
	case r.Method == http.MethodGet && strings.HasPrefix(name, "recipe/"):
		for _, rec := range s.recipes {
			if rec["uid"] == strings.TrimPrefix(name, "recipe/") {
				result(w, rec)
				return
			}
		}
		fail(w, http.StatusOK, "Recipe not found.")
	case r.Method == http.MethodGet && s.rows[name] != nil:
		result(w, live(s.rows[name]))
	case r.Method == http.MethodPost && writable[name]:
		s.write(w, r, name)
	default:
		fail(w, http.StatusNotFound, "Not found.")
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		fail(w, http.StatusOK, "Unrecognized client.")
		return
	}
	if r.PostFormValue("email") != Email {
		fail(w, http.StatusOK, "Account not found.")
		return
	}
	if r.PostFormValue("password") != Password {
		fail(w, http.StatusOK, "Invalid password.")
		return
	}
	s.tokenSeq++
	s.token = fmt.Sprintf("fixture-token-%d-do-not-log", s.tokenSeq)
	result(w, row{"token": s.token})
}

// write accepts the only body shape Paprika accepts: multipart part "data"
// holding a gzipped JSON array of complete objects.
func (s *Server) write(w http.ResponseWriter, r *http.Request, collection string) {
	file, _, err := r.FormFile("data")
	if err != nil {
		fail(w, http.StatusBadRequest, "Missing data.")
		return
	}
	defer file.Close()
	zr, err := gzip.NewReader(file)
	if err != nil {
		fail(w, http.StatusBadRequest, "Invalid data.")
		return
	}
	raw, _ := io.ReadAll(zr)
	var items []row
	if err := json.Unmarshal(raw, &items); err != nil {
		fail(w, http.StatusBadRequest, "Invalid data.")
		return
	}
	for _, it := range items {
		uid, _ := it["uid"].(string)
		if !s.known(collection, uid) && !uidShape.MatchString(uid) {
			fail(w, http.StatusOK, "Invalid uid.")
			return
		}
	}
	for _, it := range items {
		s.upsert(collection, withReadDefaults(collection, it))
	}
	if s.dropAck > 0 {
		s.dropAck--
		fail(w, http.StatusBadGateway, "Bad Gateway")
		return
	}
	result(w, true)
}

func (s *Server) known(collection, uid string) bool {
	for _, r := range s.rows[collection] {
		if r["uid"] == uid {
			return true
		}
	}
	return false
}

// withReadDefaults adds the fields Paprika returns on read even though its own
// clients omit them on write.
func withReadDefaults(collection string, r row) row {
	defaults := map[string]row{
		"meals":     {"is_ingredient": false, "scale": nil},
		"groceries": {"recipe_uid": nil},
	}
	for k, v := range defaults[collection] {
		if _, ok := r[k]; !ok {
			r[k] = v
		}
	}
	return r
}

func result(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(row{"result": v})
}

func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(row{"error": row{"code": 0, "message": message}})
}
