package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/pavel-alay/paprika-adapter/internal/paprika"
)

// SyncState is what the cache knows about one collection's freshness.
type SyncState struct {
	Collection    string
	Counter       sql.NullInt64 // Paprika's change counter at the last fetch
	LastAttemptAt time.Time
	LastCheckedAt time.Time // last time the cache was confirmed current against Paprika
	LastFetchedAt time.Time // last time the rows were actually downloaded
	ErrorKind     string    // set when the most recent attempt failed
	ErrorMessage  string
	Drift         paprika.Drift
}

// SyncStates returns the state of every collection that has ever been attempted.
func (s *Store) SyncStates(ctx context.Context) (map[string]SyncState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT collection, counter, last_attempt_at, last_checked_at,
		last_fetched_at, error_kind, error_message, drift FROM sync_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SyncState{}
	for rows.Next() {
		var (
			st                        SyncState
			attempt, checked, fetched sql.NullString
			kind, msg, drift          sql.NullString
		)
		if err := rows.Scan(&st.Collection, &st.Counter, &attempt, &checked, &fetched, &kind, &msg, &drift); err != nil {
			return nil, err
		}
		st.LastAttemptAt, st.LastCheckedAt, st.LastFetchedAt = parseStamp(attempt), parseStamp(checked), parseStamp(fetched)
		st.ErrorKind, st.ErrorMessage = kind.String, msg.String
		if drift.Valid {
			_ = json.Unmarshal([]byte(drift.String), &st.Drift)
		}
		out[st.Collection] = st
	}
	return out, rows.Err()
}

// MarkChecked records that the cached rows were confirmed current without a download.
func (s *Store) MarkChecked(ctx context.Context, collection string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sync_state SET last_attempt_at = ?1, last_checked_at = ?1,
		error_kind = NULL, error_message = NULL WHERE collection = ?2`, stamp(at), collection)
	return err
}

// SetDrift replaces the drift recorded for a collection without touching its sync times.
func (s *Store) SetDrift(ctx context.Context, collection string, drift paprika.Drift) error {
	d, _ := json.Marshal(drift)
	_, err := s.db.ExecContext(ctx, `UPDATE sync_state SET drift = ? WHERE collection = ?`, string(d), collection)
	return err
}

// MarkFetched records a successful download. counter is nil when Paprika's
// counter must not be trusted yet, which forces a fetch on the next sync.
func (s *Store) MarkFetched(ctx context.Context, collection string, counter *int64, drift paprika.Drift, at time.Time) error {
	d, _ := json.Marshal(drift)
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_state (collection, counter, last_attempt_at, last_checked_at, last_fetched_at, drift)
		VALUES (?1, ?2, ?3, ?3, ?3, ?4)
		ON CONFLICT(collection) DO UPDATE SET counter = excluded.counter,
			last_attempt_at = excluded.last_attempt_at, last_checked_at = excluded.last_checked_at,
			last_fetched_at = excluded.last_fetched_at, drift = excluded.drift, error_kind = NULL, error_message = NULL`,
		collection, counter, stamp(at), string(d))
	return err
}

// MarkFailed records a failed attempt and leaves the cached rows untouched.
func (s *Store) MarkFailed(ctx context.Context, collection, kind, message string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_state (collection, last_attempt_at, error_kind, error_message)
		VALUES (?1, ?2, ?3, ?4)
		ON CONFLICT(collection) DO UPDATE SET last_attempt_at = excluded.last_attempt_at,
			error_kind = excluded.error_kind, error_message = excluded.error_message`,
		collection, stamp(at), kind, message)
	return err
}

// Proposal is a write that has been prepared but needs an explicit commit.
type Proposal struct {
	ID        string
	Kind      string
	Payload   json.RawMessage
	State     string
	Result    json.RawMessage
	CreatedAt time.Time
	ExpiresAt time.Time
}

const (
	ProposalPending   = "pending"
	ProposalCommitted = "committed"
)

func (s *Store) SaveProposal(ctx context.Context, p Proposal) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO proposals (id, kind, payload, state, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.Kind, string(p.Payload), ProposalPending, stamp(p.CreatedAt), stamp(p.ExpiresAt))
	return err
}

func (s *Store) Proposal(ctx context.Context, id string) (Proposal, bool, error) {
	var (
		p                Proposal
		payload          string
		result           sql.NullString
		created, expires sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, payload, state, result, created_at, expires_at FROM proposals WHERE id = ?`, id).
		Scan(&p.ID, &p.Kind, &payload, &p.State, &result, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, false, nil
	}
	if err != nil {
		return Proposal{}, false, err
	}
	p.Payload, p.Result = json.RawMessage(payload), json.RawMessage(result.String)
	p.CreatedAt, p.ExpiresAt = parseStamp(created), parseStamp(expires)
	return p, true, nil
}

// CommitProposal stores the verified outcome so a repeated commit can return it
// without touching Paprika again.
func (s *Store) CommitProposal(ctx context.Context, id string, result json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `UPDATE proposals SET state = ?, result = ? WHERE id = ?`, ProposalCommitted, string(result), id)
	return err
}
