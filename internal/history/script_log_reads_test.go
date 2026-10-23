package history

import (
	"strings"
	"testing"
	"time"
)

// storedScriptLog reads script_log straight from the table. No repository read
// selects the column, so this is the only way a test can see what AddTx wrote.
func storedScriptLog(t *testing.T, repo *Repository, nzoID string) []byte {
	t.Helper()
	var log []byte
	if err := repo.DB().QueryRowContext(t.Context(),
		`SELECT script_log FROM history WHERE nzo_id = ?`, nzoID).Scan(&log); err != nil {
		t.Fatalf("read script_log for %s: %v", nzoID, err)
	}
	return log
}

// TestReads_DoNotLoadScriptLog pins that no read loads script_log, which can be
// 512 KiB per row, while AddTx still stores it. Get serves the retention
// re-read and mode=history&nzo_ids; Search serves the paged history view and
// the duplicate-NZB MD5 lookup; ExpiredEntries serves retention.
func TestReads_DoNotLoadScriptLog(t *testing.T) {
	t.Parallel()
	_, repo := openTestDB(t)
	ctx := t.Context()

	log := []byte(strings.Repeat("script output\n", 64))
	err := repo.Add(ctx, Entry{
		NzoID: "with-log", Name: "n", Status: "Completed", MD5Sum: "abc",
		Completed: time.Now().AddDate(0, 0, -10), ScriptLog: log, ScriptLine: "ran",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := storedScriptLog(t, repo, "with-log"); string(got) != string(log) {
		t.Fatalf("stored script_log = %d bytes, want the %d AddTx was given", len(got), len(log))
	}

	check := func(read string, e Entry) {
		t.Helper()
		if len(e.ScriptLog) != 0 {
			t.Errorf("%s loaded %d bytes of script_log", read, len(e.ScriptLog))
		}
		if e.ScriptLine != "ran" {
			t.Errorf("%s: ScriptLine = %q, want it kept", read, e.ScriptLine)
		}
	}

	got, err := repo.Get(ctx, "with-log")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	check("Get", *got)

	for name, opts := range map[string]SearchOptions{
		"Search page":      {},
		"Search md5 probe": {MD5Sum: "abc"},
	} {
		found, err := repo.Search(ctx, opts)
		if err != nil || len(found) != 1 {
			t.Fatalf("%s = %v, %v, want 1 entry", name, found, err)
		}
		check(name, found[0])
	}

	exp, err := repo.ExpiredEntries(ctx, 1, 1)
	if err != nil || len(exp) != 1 {
		t.Fatalf("ExpiredEntries = %v, %v, want 1 entry", exp, err)
	}
	check("ExpiredEntries", exp[0])
}
