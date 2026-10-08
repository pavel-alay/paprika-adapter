package syncer

import (
	"context"
	"time"

	"paprika-adapter/internal/paprika"
)

const (
	StateFresh = "fresh" // confirmed against Paprika within MaxAge
	StateStale = "stale" // cached rows exist but could not be confirmed recently
	StateEmpty = "never_synced"
)

// Freshness tells a caller how far to trust one cached collection.
type Freshness struct {
	Collection   string         `json:"collection" jsonschema:"Paprika collection the data came from"`
	State        string         `json:"state" jsonschema:"fresh = confirmed against Paprika recently; stale = cached copy that could not be confirmed (see error); never_synced = nothing cached yet"`
	LastSyncedAt string         `json:"last_synced_at,omitempty" jsonschema:"RFC 3339 UTC time the cached copy was last confirmed current"`
	AgeSeconds   *int64         `json:"age_seconds,omitempty" jsonschema:"seconds since last_synced_at"`
	Rows         int            `json:"rows" jsonschema:"rows cached for this collection"`
	Error        *SyncError     `json:"error,omitempty" jsonschema:"why the most recent sync attempt failed, if it did"`
	Drift        *paprika.Drift `json:"protocol_drift,omitempty" jsonschema:"fields Paprika added or stopped sending compared to what the adapter was built against"`
}

// SyncError is the recorded reason a sync attempt failed.
type SyncError struct {
	Code    string `json:"code" jsonschema:"auth_failed, upstream_unavailable, protocol_changed, rejected, not_found or cache_error"`
	Message string `json:"message"`
	At      string `json:"at" jsonschema:"RFC 3339 UTC time of the failed attempt"`
}

// Freshness reports the cached state of the named collections without any network I/O.
func (s *Syncer) Freshness(ctx context.Context, collections ...string) []Freshness {
	ctx = context.WithoutCancel(ctx)
	states, err := s.Store.SyncStates(ctx)
	counts, cerr := s.Store.RowCounts(ctx)
	if err == nil {
		err = cerr
	}
	out := make([]Freshness, 0, len(collections))
	now := s.Now()
	for _, c := range collections {
		f := Freshness{Collection: c, State: StateEmpty}
		if err != nil {
			f.Error = &SyncError{Code: "cache_error", Message: err.Error(), At: now.UTC().Format(time.RFC3339)}
			out = append(out, f)
			continue
		}
		st := states[c]
		f.Rows = counts[c]
		if f.Rows > 0 {
			f.State = StateStale
		}
		if !st.LastCheckedAt.IsZero() {
			age := int64(now.Sub(st.LastCheckedAt).Seconds())
			f.LastSyncedAt, f.AgeSeconds = st.LastCheckedAt.UTC().Format(time.RFC3339), &age
			f.State = StateStale
			if now.Sub(st.LastCheckedAt) <= s.MaxAge {
				f.State = StateFresh
			}
		}
		if st.ErrorKind != "" {
			f.Error = &SyncError{Code: st.ErrorKind, Message: st.ErrorMessage, At: st.LastAttemptAt.UTC().Format(time.RFC3339)}
		}
		if !st.Drift.Empty() {
			f.Drift = &st.Drift
		}
		out = append(out, f)
	}
	return out
}
