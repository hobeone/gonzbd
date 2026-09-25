package durability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// reclaimState is one way a job can be reachable, and which of its rows the
// reclaim rule must keep for it.
type reclaimState struct {
	id        string
	queued    bool
	history   constants.Status // "" for no history entry
	keepAll   bool             // every table's rows survive
	keepsRuns bool             // durable_runs alone survives
}

var reclaimStates = []reclaimState{
	{id: "queued", queued: true, keepAll: true},
	{id: "queued-and-failed", queued: true, history: constants.StatusFailed, keepAll: true},
	{id: "failed", history: constants.StatusFailed, keepsRuns: true},
	{id: "completed", history: constants.StatusCompleted},
	{id: "neither"},
}

// seedReclaimStates gives every state a row in every per-job table, a queue
// row where the state has one, and a history entry written through
// internal/history's own Add — so a change to history's columns or status
// values fails here, not silently in the rule.
func seedReclaimStates(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	hdb, err := history.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hdb.Close() })
	repo := history.NewRepository(hdb)
	db := repo.DB()
	st := NewStore(db)
	for _, s := range reclaimStates {
		if err := st.Admit(ctx, s.id, []uint8{0}); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveProgress(ctx, []JobProgress{{JobID: s.id, FailedArticles: []int{3}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.commit(ctx, s.id, []DurableArticle{{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 10, CRC32: 1}}); err != nil {
			t.Fatal(err)
		}
		if s.queued {
			if _, err := db.Exec(`INSERT INTO dispatch_jobs (id, sort_key, name) VALUES (?, 0, ?)`, s.id, s.id); err != nil {
				t.Fatal(err)
			}
		}
		if s.history != "" {
			if err := repo.Add(ctx, history.Entry{NzoID: s.id, Name: s.id, Status: string(s.history)}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

func assertReclaimed(t *testing.T, db *sql.DB, via string) {
	t.Helper()
	for _, s := range reclaimStates {
		for _, table := range perJobTables {
			want := 0
			if s.keepAll || (s.keepsRuns && table.name == "durable_runs") {
				want = 1
			}
			if got := countRows(t, db, table.name, s.id); got != want {
				t.Errorf("%s: job %q has %d %s rows, want %d", via, s.id, got, table.name, want)
			}
		}
	}
}

// TestReclaim_AppliesTheRuleToEveryState pins the reclaim rule's every branch
// against the real schema, through both entry points: they share one text, and
// this is what shows it. The columns are perJobTables itself, so a table added
// there is checked here with no change to this test.
func TestReclaim_AppliesTheRuleToEveryState(t *testing.T) {
	ctx := context.Background()

	t.Run("SweepOrphans", func(t *testing.T) {
		db := seedReclaimStates(t)
		if err := NewStore(db).SweepOrphans(ctx); err != nil {
			t.Fatal(err)
		}
		assertReclaimed(t, db, "SweepOrphans")
	})

	t.Run("Reclaim", func(t *testing.T) {
		db := seedReclaimStates(t)
		st := NewStore(db)
		for _, s := range reclaimStates {
			if err := st.Reclaim(ctx, s.id); err != nil {
				t.Fatal(err)
			}
		}
		assertReclaimed(t, db, "Reclaim")
	})
}

// TestReclaim_TouchesOnlyTheNamedJobs pins the filter: reclaiming one
// unreachable job leaves another unreachable job's rows alone.
func TestReclaim_TouchesOnlyTheNamedJobs(t *testing.T) {
	ctx := context.Background()
	db := seedReclaimStates(t)
	if err := NewStore(db).Reclaim(ctx, "neither"); err != nil {
		t.Fatal(err)
	}
	for _, table := range perJobTables {
		if n := countRows(t, db, table.name, "completed"); n != 1 {
			t.Errorf("%s: an unnamed job lost its rows (%d left); Reclaim is not filtered", table.name, n)
		}
	}
}

// TestReclaim_IgnoresANullQueueID pins NOT EXISTS over NOT IN. A NULL id in
// dispatch_jobs (the column is a TEXT primary key, which SQLite lets be NULL)
// makes every NOT IN false, so the rule would reclaim nothing.
func TestReclaim_IgnoresANullQueueID(t *testing.T) {
	ctx := context.Background()
	db := seedReclaimStates(t)
	if _, err := db.Exec(`INSERT INTO dispatch_jobs (id, sort_key, name) VALUES (NULL, 0, 'null')`); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(db).SweepOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	assertReclaimed(t, db, "SweepOrphans with a NULL queue id")
}

// TestReclaim_ReportsAClosedDatabase covers both entry points' failure path:
// a departure that could not reclaim must be able to say so.
func TestReclaim_ReportsAClosedDatabase(t *testing.T) {
	ctx := context.Background()
	st := closedStore(t)
	if err := st.Reclaim(ctx, "j"); err == nil {
		t.Error("Reclaim on a closed database = nil, want an error")
	}
	if err := st.SweepOrphans(ctx); err == nil {
		t.Error("SweepOrphans on a closed database = nil, want an error")
	}
}

// TestPerJobTables_CoversEveryJobKeyedTable fails when the schema gains a
// table keyed by job_id that the reclaim rule does not cover. history_job_files
// is the one exception: internal/history owns it and deletes it with its entry.
func TestPerJobTables_CoversEveryJobKeyedTable(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`
		SELECT m.name FROM sqlite_master m, pragma_table_info(m.name) c
		 WHERE m.type = 'table' AND c.name = 'job_id' ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var inSchema []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "history_job_files" {
			inSchema = append(inSchema, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	covered := make([]string, 0, len(perJobTables))
	for _, pt := range perJobTables {
		covered = append(covered, pt.name)
	}
	slices.Sort(covered)
	if !slices.Equal(inSchema, covered) {
		t.Errorf("tables keyed by job_id = %v, perJobTables = %v; a job_id table the "+
			"reclaim rule does not cover is never reclaimed", inSchema, covered)
	}
}

// TestRuleStatement_TheFilterIsTheOnlyDifference is what "one text" means:
// for every table, Reclaim's statement is SweepOrphans' with the id filter
// appended, and its arguments are SweepOrphans' with the ids appended.
func TestRuleStatement_TheFilterIsTheOnlyDifference(t *testing.T) {
	ids := []string{"a", "b"}
	for _, table := range perJobTables {
		sweep, reclaim := ruleStatement(table, 0), ruleStatement(table, len(ids))
		if !strings.HasPrefix(reclaim, sweep) || !strings.Contains(reclaim[len(sweep):], "job_id IN (?,?)") {
			t.Errorf("%s: Reclaim's statement is not SweepOrphans' plus the id filter:\n%s\n---\n%s",
				table.name, sweep, reclaim)
		}
		sweepArgs, reclaimArgs := ruleArgs(table, nil), ruleArgs(table, ids)
		if len(reclaimArgs) != len(sweepArgs)+len(ids) {
			t.Errorf("%s: args %v and %v differ by more than the ids", table.name, sweepArgs, reclaimArgs)
		}
		// One placeholder per argument, in both forms: a mismatch is a bound
		// parameter landing in the wrong clause.
		for _, q := range []struct {
			sql  string
			args []any
		}{{sweep, sweepArgs}, {reclaim, reclaimArgs}} {
			if got := strings.Count(q.sql, "?"); got != len(q.args) {
				t.Errorf("%s: %d placeholders for %d arguments in:\n%s", table.name, got, len(q.args), q.sql)
			}
		}
	}
}

// TestReclaim_ChunksLargeIDLists pins that a bulk history delete stays under
// SQLite's host-parameter limit: more ids than one statement may name, in one
// transaction, and every unreachable one is reclaimed.
func TestReclaim_ChunksLargeIDLists(t *testing.T) {
	ctx := context.Background()
	db := seedReclaimStates(t)
	st := NewStore(db)
	ids := make([]string, 0, reclaimChunk*2+3)
	for i := range reclaimChunk*2 + 2 {
		ids = append(ids, fmt.Sprintf("bulk-%04d", i))
	}
	if err := st.Admit(ctx, ids[0], []uint8{0}); err != nil {
		t.Fatal(err)
	}
	ids = append(ids, "neither")

	if err := st.Reclaim(ctx, ids[0], ids[1:]...); err != nil {
		t.Fatalf("Reclaim over %d ids: %v", len(ids), err)
	}
	for _, id := range []string{ids[0], "neither"} {
		if n := countRows(t, db, "job_files", id); n != 0 {
			t.Errorf("%s has %d job_files rows after a chunked reclaim, want 0", id, n)
		}
	}
	if n := countRows(t, db, "job_files", "queued"); n != 1 {
		t.Errorf("a queued job lost its rows to a chunked reclaim (%d left)", n)
	}
}

// TestInTx_RollsBackWhenTheRuleFails pins the atomicity reclaim depends on: a
// statement that fails takes back the ones that ran before it, so a rule that
// cannot read history deletes nothing rather than some tables' rows.
func TestInTx_RollsBackWhenTheRuleFails(t *testing.T) {
	ctx := context.Background()
	db := seedReclaimStates(t)
	st := NewStore(db)
	boom := errors.New("boom")
	err := st.inTx(ctx, "test", func(tx *sql.Tx) error {
		if err := applyRule(ctx, tx, nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("inTx = %v, want the function's error", err)
	}
	if n := countRows(t, db, "job_files", "neither"); n != 1 {
		t.Errorf("an unreachable job has %d job_files rows after a failed transaction, want 1 — "+
			"the deletes before the failure were committed", n)
	}
}

// TestReclaim_ReportsAStatementThatCannotReadHistory pins the failure a
// departure most plausibly meets mid-rule: the durable_runs statement reads
// history, and when it cannot, Reclaim reports it and deletes nothing.
func TestReclaim_ReportsAStatementThatCannotReadHistory(t *testing.T) {
	ctx := context.Background()
	db := seedReclaimStates(t)
	if _, err := db.Exec(`DROP TABLE history`); err != nil {
		t.Fatal(err)
	}
	err := NewStore(db).Reclaim(ctx, "neither")
	if err == nil || !strings.Contains(err.Error(), "reclaim durable_runs") {
		t.Fatalf("Reclaim = %v, want an error naming the durable_runs statement", err)
	}
	if n := countRows(t, db, "job_files", "neither"); n != 1 {
		t.Errorf("%d job_files rows after a failed reclaim, want 1", n)
	}
}
