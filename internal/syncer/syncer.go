// Package syncer pulls Paprika data into the cache. It only ever reads from
// Paprika; a failed sync leaves the cache as it was and records why.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"paprika-adapter/internal/paprika"
	"paprika-adapter/internal/store"
)

// Syncer keeps the cache current. Syncs are serialised.
type Syncer struct {
	Client *paprika.Client
	Store  *store.Store
	Log    *slog.Logger
	Now    func() time.Time

	MaxAge        time.Duration // cached data older than this is re-checked before it is served
	CounterTrust  time.Duration // a collection is re-downloaded this often even if its counter did not move
	FailureWait   time.Duration // after a failed attempt, do not try again for this long
	Budget        time.Duration // upper bound for one on-demand sync
	RecipeWorkers int

	busy chan struct{} // holds one token while a sync runs
}

// New returns a Syncer with defaults suited to one household account.
func New(client *paprika.Client, st *store.Store, log *slog.Logger) *Syncer {
	return &Syncer{
		Client: client, Store: st, Log: log, Now: time.Now,
		MaxAge: 5 * time.Minute, CounterTrust: 6 * time.Hour, FailureWait: 30 * time.Second,
		Budget: 25 * time.Second, RecipeWorkers: 4, busy: make(chan struct{}, 1),
	}
}

// All names every collection the adapter caches, in sync order. Recipes come
// last: they are the slow part, and everything else should not wait for them.
func All() []string {
	out := make([]string, 0, len(paprika.Collections)+1)
	for _, c := range paprika.Collections {
		out = append(out, string(c))
	}
	return append(out, paprika.Recipes)
}

// Ensure refreshes the named collections if they are older than MaxAge, within
// Budget, and reports how fresh each one is afterwards. It never fails: when
// Paprika cannot be reached the cached rows stay and the report says so.
func (s *Syncer) Ensure(ctx context.Context, collections ...string) []Freshness {
	ctx, cancel := context.WithTimeout(ctx, s.Budget)
	defer cancel()
	s.sync(ctx, collections, false)
	return s.Freshness(ctx, collections...)
}

// Refresh re-checks the named collections now, regardless of age and without a budget.
func (s *Syncer) Refresh(ctx context.Context, collections ...string) []Freshness {
	s.sync(ctx, collections, true)
	return s.Freshness(ctx, collections...)
}

// Run refreshes everything now and then every interval until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	for {
		s.sync(ctx, All(), true)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (s *Syncer) sync(ctx context.Context, collections []string, force bool) {
	// A caller with a budget must not queue behind a long first sync.
	select {
	case s.busy <- struct{}{}:
		defer func() { <-s.busy }()
	case <-ctx.Done():
		return
	}
	states, err := s.Store.SyncStates(ctx)
	if err != nil {
		s.Log.Error("cache unreadable", "error", err.Error())
		return
	}
	now := s.Now()
	var due []string
	for _, c := range collections {
		st := states[c]
		failedRecently := st.ErrorKind != "" && now.Sub(st.LastAttemptAt) < s.FailureWait
		if force || (now.Sub(st.LastCheckedAt) > s.MaxAge && !failedRecently) {
			due = append(due, c)
		}
	}
	if len(due) == 0 {
		return
	}
	counters, err := s.Client.Status(ctx)
	if err != nil {
		for _, c := range due {
			s.failed(ctx, c, err)
		}
		return
	}
	for _, c := range due {
		st := states[c]
		counter, known := counters[c]
		unchanged := known && st.Counter.Valid && st.Counter.Int64 == counter
		if unchanged && !st.LastFetchedAt.IsZero() && now.Sub(st.LastFetchedAt) < s.CounterTrust {
			if err := s.Store.MarkChecked(ctx, c, now); err != nil {
				s.Log.Error("cache write failed", "collection", c, "error", err.Error())
			}
			continue
		}
		var ptr *int64
		if known {
			ptr = &counter
		}
		if err := s.fetch(ctx, c, ptr, st); err != nil {
			s.failed(ctx, c, err)
		}
	}
}

func (s *Syncer) fetch(ctx context.Context, collection string, counter *int64, prev store.SyncState) error {
	if collection == paprika.Recipes {
		return s.fetchRecipes(ctx, counter, prev)
	}
	if _, err := s.Pull(ctx, paprika.Collection(collection), counter); err != nil {
		return err
	}
	if collection == string(paprika.Categories) {
		return s.Store.RebuildSearchIndex(ctx) // recipes are indexed under their category names
	}
	return nil
}

// Pull downloads one collection straight from Paprika, replaces its cached rows
// and returns what Paprika sent. Write verification uses it to read back.
func (s *Syncer) Pull(ctx context.Context, col paprika.Collection, counter *int64) ([]paprika.Raw, error) {
	rows, err := s.Client.List(ctx, col)
	if err != nil {
		return nil, err
	}
	if err := s.Store.ReplaceEntities(ctx, col, rows); err != nil {
		return nil, err
	}
	drift := paprika.DetectDrift(string(col), rows)
	if !drift.Empty() {
		s.Log.Warn("paprika protocol drift", "collection", string(col), "unknown", drift.Unknown, "missing", drift.Missing)
	}
	return rows, s.Store.MarkFetched(ctx, string(col), counter, drift, s.Now())
}

// fetchRecipes downloads only recipes whose hash differs from the cached one.
// Each recipe is stored as it arrives, so an interrupted sync resumes where it stopped.
func (s *Syncer) fetchRecipes(ctx context.Context, counter *int64, prev store.SyncState) error {
	refs, err := s.Client.RecipeRefs(ctx)
	if err != nil {
		return err
	}
	cached, err := s.Store.RecipeHashes(ctx)
	if err != nil {
		return err
	}
	var wanted []paprika.RecipeRef
	listed := map[string]bool{}
	for _, r := range refs {
		listed[r.UID] = true
		if cached[r.UID] != r.Hash {
			wanted = append(wanted, r)
		}
	}
	var removed []string
	for uid := range cached {
		if !listed[uid] {
			removed = append(removed, uid)
		}
	}

	var (
		mu       sync.Mutex
		fetched  []paprika.Raw
		firstErr error
		wg       sync.WaitGroup
		jobs     = make(chan paprika.RecipeRef)
	)
	for range min(s.RecipeWorkers, len(wanted)) {
		wg.Go(func() {
			for ref := range jobs {
				raw, err := s.Client.Recipe(ctx, ref.UID)
				if err == nil {
					err = s.Store.PutRecipe(ctx, raw, ref.Hash, s.Now())
				}
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if err == nil {
					fetched = append(fetched, raw)
				}
				mu.Unlock()
			}
		})
	}
	for _, ref := range wanted {
		jobs <- ref
	}
	close(jobs)
	wg.Wait()

	if err := s.Store.DeleteRecipes(ctx, removed); err != nil {
		return err
	}
	if pending := len(wanted) - len(fetched); pending > 0 {
		return &paprika.Error{
			Kind: kindOr(firstErr, paprika.KindUnavailable), Op: "sync recipes", Err: firstErr,
			Message: fmt.Sprintf("%d of %d changed recipes could not be fetched; the rest are cached", pending, len(wanted)),
		}
	}
	drift := prev.Drift
	if len(fetched) > 0 {
		drift = paprika.DetectDrift(paprika.Recipes, fetched)
	}
	if !drift.Empty() {
		s.Log.Warn("paprika protocol drift", "collection", paprika.Recipes, "unknown", drift.Unknown, "missing", drift.Missing)
	}
	s.Log.Info("recipes synced", "listed", len(refs), "fetched", len(fetched), "removed", len(removed))
	return s.Store.MarkFetched(ctx, paprika.Recipes, counter, drift, s.Now())
}

func (s *Syncer) failed(ctx context.Context, collection string, err error) {
	kind, msg := string(kindOr(err, "cache_error")), err.Error()
	var pe *paprika.Error
	if errors.As(err, &pe) {
		msg = pe.Message
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		kind, msg = string(paprika.KindUnavailable), "sync did not finish in time; it resumes on the next call"
	}
	s.Log.Warn("sync failed", "collection", collection, "kind", kind, "message", msg)
	// The request context may be the reason for the failure, so record it with a fresh one.
	if err := s.Store.MarkFailed(context.WithoutCancel(ctx), collection, kind, msg, s.Now()); err != nil {
		s.Log.Error("cache write failed", "collection", collection, "error", err.Error())
	}
}

func kindOr(err error, fallback paprika.Kind) paprika.Kind {
	if k := paprika.KindOf(err); k != "" {
		return k
	}
	return fallback
}
