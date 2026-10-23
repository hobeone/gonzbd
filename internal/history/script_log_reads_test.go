package history

import (
	"strings"
	"testing"
	"time"
)

// TestListReads_DoNotLoadScriptLog pins which reads select script_log. The log
// can be 512 KiB per row, so Search (the paged history view and the
// duplicate-NZB MD5 lookup) and ExpiredEntries must not pull it, while Get
// still returns it. The assertion is on the returned Entry, which is what a
// query that re-added the column would change.
func TestListReads_DoNotLoadScriptLog(t *testing.T) {
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

	full, err := repo.Get(ctx, "with-log")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(full.ScriptLog) != string(log) {
		t.Errorf("Get ScriptLog = %d bytes, want the stored %d", len(full.ScriptLog), len(log))
	}

	for name, opts := range map[string]SearchOptions{
		"page":      {},
		"md5 probe": {MD5Sum: "abc"},
	} {
		got, err := repo.Search(ctx, opts)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: Search = %v, %v, want 1 entry", name, got, err)
		}
		if len(got[0].ScriptLog) != 0 {
			t.Errorf("%s: Search loaded %d bytes of script_log", name, len(got[0].ScriptLog))
		}
		if got[0].ScriptLine != "ran" {
			t.Errorf("%s: ScriptLine = %q, want it kept", name, got[0].ScriptLine)
		}
	}

	exp, err := repo.ExpiredEntries(ctx, 1, 1)
	if err != nil || len(exp) != 1 {
		t.Fatalf("ExpiredEntries = %v, %v, want 1 entry", exp, err)
	}
	if len(exp[0].ScriptLog) != 0 {
		t.Errorf("ExpiredEntries loaded %d bytes of script_log", len(exp[0].ScriptLog))
	}
}

// TestColumnLists_DifferOnlyByScriptLog pins that the two SELECT lists are one
// definition: removing script_log from allColumns yields listColumns exactly.
func TestColumnLists_DifferOnlyByScriptLog(t *testing.T) {
	t.Parallel()
	if !strings.Contains(allColumns, "script_log") {
		t.Fatal("allColumns lost script_log")
	}
	if strings.Contains(listColumns, "script_log") {
		t.Error("listColumns selects script_log")
	}
	if got, want := len(strings.Split(listColumns, ",")), len(strings.Split(allColumns, ","))-1; got != want {
		t.Errorf("listColumns has %d columns, want %d (allColumns minus script_log)", got, want)
	}
}

// TestScanEntry_ListShapeMapsColumnsByPosition is the listRow counterpart of
// TestScanEntry_MapsColumnsByPosition: with script_log absent every column after
// it sits one position earlier.
func TestScanEntry_ListShapeMapsColumnsByPosition(t *testing.T) {
	t.Parallel()
	got, err := scanEntry(fixedScanner{at: map[int]any{
		8:  "Failed",
		9:  "SABnzbd_nzo_xyz",
		18: "par2 repair fail", // fail_message
		19: "http://info",      // url_info
		20: int64(4242),        // bytes
		24: int64(1),           // archive
		25: int64(1700000000),  // time_added
	}}, listRow)
	if err != nil {
		t.Fatalf("scanEntry: %v", err)
	}
	if got.Status != "Failed" || got.NzoID != "SABnzbd_nzo_xyz" ||
		got.FailMessage != "par2 repair fail" || got.URLInfo != "http://info" ||
		got.Bytes != 4242 || got.Archive != 1 || got.TimeAdded.Unix() != 1700000000 {
		t.Errorf("list-shape columns misplaced: %+v", got)
	}
}
