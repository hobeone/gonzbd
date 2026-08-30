package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

// Store persists the dispatcher's queue in the dispatch_jobs table.
type Store struct {
	db *sql.DB
}

var _ dispatch.Store = (*Store)(nil)

// New returns a Store over db. The caller owns db and its lifetime; Store
// neither opens nor closes it.
func New(db *sql.DB) *Store { return &Store{db: db} }

// columns is the column list Load scans and Save writes, in one place so the
// two cannot drift into disagreeing about order. A mismatch between them would
// not fail to compile — every value is an integer or a string — it would
// silently transpose fields.
const columns = `id, sort_key, name, category, priority, bytes,
	verify, repair, unpack, delete_ok,
	state, next, activity, outcome, assessed, intent`

// Load returns every stored job in queue order.
//
// The ID tiebreak makes the result total rather than merely mostly-ordered:
// SQLite is free to return rows sharing a sort_key in any order, and
// dispatch.restore sorts with the same tiebreak so the two agree.
func (s *Store) Load(ctx context.Context) ([]dispatch.Persisted, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+columns+` FROM dispatch_jobs ORDER BY sort_key ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("dispatch/store: load: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []dispatch.Persisted
	for rows.Next() {
		var (
			p                              dispatch.Persisted
			verify, repair, unpack, delOK  int
			state, next, activity, outcome int
			assessed, intent               int
		)
		if err := rows.Scan(
			&p.ID, &p.SortKey, &p.Header.Name, &p.Header.Category, &p.Header.Priority, &p.Header.Bytes,
			&verify, &repair, &unpack, &delOK,
			&state, &next, &activity, &outcome, &assessed, &intent,
		); err != nil {
			return nil, fmt.Errorf("dispatch/store: load: scan: %w", err)
		}
		p.Policy = job.Policy{
			Verify: verify != 0, Repair: repair != 0,
			Unpack: unpack != 0, Delete: delOK != 0,
		}
		// Range-check before narrowing to uint8. A plain conversion truncates,
		// and truncation is worse here than a wrong value: 256 becomes 0, which
		// is StateUnset — a LEGAL persisted position meaning "never ran". A
		// corrupt row would therefore restore as a plausible queued job rather
		// than failing, and reconstruct could not tell the difference, because
		// StateUnset is exactly the shape it accepts without opening an
		// attempt.
		enums, err := narrowAll(p.ID, state, next, activity, outcome, intent)
		if err != nil {
			return nil, err
		}
		p.State = job.StateView{
			State:    job.State(enums[0]),
			Next:     job.State(enums[1]),
			Activity: job.Activity(enums[2]),
			Outcome:  job.Outcome(enums[3]),
			Assessed: assessed != 0,
		}
		p.Intent = job.Intent(enums[4])
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dispatch/store: load: %w", err)
	}
	return out, nil
}

// Save writes one job's row, inserting or replacing it.
//
// It is an unconditional upsert over every column rather than a
// read-then-decide: persistIfChanged has already established that the row
// differs from what was last written, so a second comparison here would be
// work with no consumer — and it would put a second opinion about "has this
// changed?" behind the one that already owns the question.
func (s *Store) Save(ctx context.Context, p dispatch.Persisted) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dispatch_jobs (`+columns+`)
		 VALUES (?,?,?,?,?,?, ?,?,?,?, ?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   sort_key=excluded.sort_key, name=excluded.name,
		   category=excluded.category, priority=excluded.priority,
		   bytes=excluded.bytes,
		   verify=excluded.verify, repair=excluded.repair,
		   unpack=excluded.unpack, delete_ok=excluded.delete_ok,
		   state=excluded.state, next=excluded.next,
		   activity=excluded.activity, outcome=excluded.outcome,
		   assessed=excluded.assessed, intent=excluded.intent`,
		p.ID, p.SortKey, p.Header.Name, p.Header.Category, p.Header.Priority, p.Header.Bytes,
		b2i(p.Policy.Verify), b2i(p.Policy.Repair), b2i(p.Policy.Unpack), b2i(p.Policy.Delete),
		int(p.State.State), int(p.State.Next), int(p.State.Activity), int(p.State.Outcome),
		b2i(p.State.Assessed), int(p.Intent),
	)
	if err != nil {
		return fmt.Errorf("dispatch/store: save %s: %w", p.ID, err)
	}
	return nil
}

// Delete removes a job's row.
//
// Deleting a row that is not there is not an error, and that is a contract
// rather than an accident: evictCancelledNeverRun (dispatch/tick.go) treats a
// failed delete as the job's pass being over either way, so reporting absence
// would turn a redundant evict into a logged failure on every later tick.
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dispatch_jobs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("dispatch/store: delete %s: %w", id, err)
	}
	return nil
}

// narrowAll converts each stored enum value to the uint8 the job package's
// types are built on, refusing any that would not survive the narrowing.
//
// It reports the offending value rather than clamping: a row outside the range
// cannot have been written by this package, so the honest answer is to fail the
// load rather than invent a position for it.
func narrowAll(id string, vs ...int) ([]uint8, error) {
	out := make([]uint8, len(vs))
	for i, v := range vs {
		if v < 0 || v > math.MaxUint8 {
			return nil, fmt.Errorf("dispatch/store: load %s: enum value %d is out of range", id, v)
		}
		out[i] = uint8(v)
	}
	return out, nil
}

// b2i maps a Go bool to the integer SQLite stores. The columns are INTEGER
// rather than a boolean type because SQLite has none.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
