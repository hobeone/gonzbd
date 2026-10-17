package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestCompareScalar(t *testing.T) {
	tests := []struct {
		name          string
		got, op, want string
		ok            bool
	}{
		{"ge equal", "5", "ge", "5", true},
		{"ge less", "4", "ge", "5", false},
		{"ge numeric not lexical", "10", "ge", "9", true},
		{"gt equal", "5", "gt", "5", false},
		{"gt greater", "6", "gt", "5", true},
		{"ge non-numeric got", "abc", "ge", "1", false},
		{"ge empty got", "", "ge", "1", false},
		{"ge infinite got", "+Inf", "ge", "1", false},
		{"gt NaN got", "NaN", "gt", "1", false},
		{"ge NaN want", "5", "ge", "NaN", false},
		{"eq match", "Completed", "eq", "Completed", true},
		{"eq is string compare", "5.0", "eq", "5", false},
		{"eq mismatch", "Failed", "eq", "Completed", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareScalar(tt.got, tt.op, tt.want); got != tt.ok {
				t.Fatalf("compareScalar(%q,%q,%q) = %v, want %v", tt.got, tt.op, tt.want, got, tt.ok)
			}
		})
	}
}

func TestSQLCondition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	cond := &sqlCondition{path: path, query: "SELECT count(*) FROM written_articles", op: "ge", want: "2"}
	ctx := t.Context()

	if got, ok, err := cond.check(ctx); err != nil || ok || got != "(db not found)" {
		t.Fatalf("missing db = %q,%v,%v; want (db not found),false,nil", got, ok, err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE other (x int)"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := cond.check(ctx); err != nil || ok || !strings.HasPrefix(got, "(query error:") {
		t.Fatalf("table not created yet = %q,%v,%v; want query error observation, no error", got, ok, err)
	}

	if _, err := db.Exec("CREATE TABLE written_articles (id int); INSERT INTO written_articles VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := cond.check(ctx); err != nil || ok || got != "1" {
		t.Fatalf("count 1 vs ge 2 = %q,%v,%v; want 1,false,nil", got, ok, err)
	}
	if _, err := db.Exec("INSERT INTO written_articles VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := cond.check(ctx); err != nil || !ok || got != "2" {
		t.Fatalf("count 2 vs ge 2 = %q,%v,%v; want 2,true,nil", got, ok, err)
	}

	eq := &sqlCondition{path: path, query: "SELECT 'Completed'", op: "eq", want: "Completed"}
	if got, ok, err := eq.check(ctx); err != nil || !ok || got != "Completed" {
		t.Fatalf("eq text = %q,%v,%v", got, ok, err)
	}
	empty := &sqlCondition{path: path, query: "SELECT ''", op: "eq", want: ""}
	if got, ok, err := empty.check(ctx); err != nil || !ok || got != "" {
		t.Fatalf("eq empty string vs empty = %q,%v,%v; want empty,true,nil", got, ok, err)
	}
	null := &sqlCondition{path: path, query: "SELECT NULL", op: "eq", want: ""}
	if got, ok, err := null.check(ctx); err != nil || ok || got != "(NULL)" {
		t.Fatalf("eq NULL vs empty = %q,%v,%v; want (NULL),false,nil", got, ok, err)
	}
}

func TestSQLCondition_ErrorClasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (n int, m int)"); err != nil {
		t.Fatal(err)
	}

	retry := []struct{ name, query string }{
		{"table not created yet", "SELECT n FROM absent"},
		{"no row yet", "SELECT n FROM t"},
	}
	for _, tt := range retry {
		t.Run("retries "+tt.name, func(t *testing.T) {
			c := &sqlCondition{path: path, query: tt.query, op: "ge", want: "1"}
			got, ok, err := c.check(t.Context())
			if err != nil || ok || !strings.HasPrefix(got, "(query error:") {
				t.Fatalf("check = %q,%v,%v; want a query-error observation and no error", got, ok, err)
			}
		})
	}

	// A row is needed so that "two columns" fails on the destination count, not on no-rows.
	if _, err := db.Exec("INSERT INTO t VALUES (1, 2)"); err != nil {
		t.Fatal(err)
	}
	fatal := []struct{ name, query string }{
		{"syntax error", "SELEC n FROM t"},
		{"unknown column", "SELECT nope FROM t"},
		{"two columns for one scalar", "SELECT n, m FROM t"},
	}
	for _, tt := range fatal {
		t.Run("fails fast on "+tt.name, func(t *testing.T) {
			c := &sqlCondition{path: path, query: tt.query, op: "ge", want: "1"}
			_, ok, err := c.check(t.Context())
			if err == nil || ok || !strings.Contains(err.Error(), "can never succeed") {
				t.Fatalf("check err = %v, ok = %v; want a can-never-succeed error", err, ok)
			}
		})
	}
}

func TestPollUntil_StopsAtOnceOnFatalError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (n int)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	cond := &sqlCondition{path: path, query: "SELEC 1", op: "ge", want: "1"}
	start := time.Now()
	_, matched, err := pollUntil(t.Context(), cond, 30*time.Second, 5*time.Millisecond)
	if err == nil || matched {
		t.Fatalf("pollUntil = matched %v, err %v; want an error", matched, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("pollUntil spun for %v on a fatal error", d)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestLogCondition_Incremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gonzbd.log")
	cond := &logCondition{path: path, re: regexp.MustCompile(`repair needed`)}
	ctx := t.Context()

	if got, ok, _ := cond.check(ctx); ok || got != "(log not found)" {
		t.Fatalf("missing log = %q,%v", got, ok)
	}

	appendFile(t, path, "starting up\nunrelated line\n")
	if _, ok, _ := cond.check(ctx); ok {
		t.Fatal("matched before the line was written")
	}
	size := int64(len("starting up\nunrelated line\n"))
	if cond.offset != size {
		t.Fatalf("offset = %d, want %d (should have consumed the file once)", cond.offset, size)
	}

	// A match split across two writes is only seen once its newline arrives.
	appendFile(t, path, "on-demand par2: repair ne")
	if _, ok, _ := cond.check(ctx); ok {
		t.Fatal("matched a partial line")
	}
	appendFile(t, path, "eded, fetching\n")
	got, ok, _ := cond.check(ctx)
	if !ok || got != "on-demand par2: repair needed, fetching" {
		t.Fatalf("split line = %q,%v", got, ok)
	}

	// Truncation resets the offset so a rotated log is read from the top.
	if err := os.WriteFile(path, []byte("repair needed again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := cond.check(ctx); !ok || got != "repair needed again" {
		t.Fatalf("after truncate = %q,%v", got, ok)
	}
}

func TestLogCondition_FromEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gonzbd.log")
	appendFile(t, path, "repair needed (from the previous run)\n")

	w := waitFlags{log: path, regex: "repair needed", fromEnd: true, set: map[string]bool{"from-end": true}}
	cond, err := w.condition()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := cond.check(t.Context()); ok {
		t.Fatalf("--from-end matched a stale line: %q", got)
	}
	appendFile(t, path, "repair needed (this run)\n")
	if got, ok, _ := cond.check(t.Context()); !ok || got != "repair needed (this run)" {
		t.Fatalf("after a fresh line = %q,%v", got, ok)
	}

	// Without --from-end the stale line matches.
	w = waitFlags{log: path, regex: "repair needed", set: map[string]bool{}}
	cond, err = w.condition()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := cond.check(t.Context()); !ok || got != "repair needed (from the previous run)" {
		t.Fatalf("without --from-end = %q,%v; want the stale line", got, ok)
	}

	// A log that does not exist when the wait starts is read from its first byte.
	later := filepath.Join(t.TempDir(), "later.log")
	w = waitFlags{log: later, regex: "ready", fromEnd: true, set: map[string]bool{"from-end": true}}
	cond, err = w.condition()
	if err != nil {
		t.Fatal(err)
	}
	appendFile(t, later, "ready\n")
	if got, ok, _ := cond.check(t.Context()); !ok || got != "ready" {
		t.Fatalf("log created after start = %q,%v", got, ok)
	}
}

func TestGlobCondition(t *testing.T) {
	root := t.TempDir()
	ctx := t.Context()

	pat := &globCondition{pattern: filepath.Join(root, "*.par2engine-*.tmp")}
	byName := &globCondition{root: filepath.Join(root, "incomplete"), name: "*.par2engine-*.tmp"}
	for _, c := range []*globCondition{pat, byName} {
		if got, ok, _ := c.check(ctx); ok || got != "(no match)" {
			t.Fatalf("empty tree = %q,%v", got, ok)
		}
	}

	nested := filepath.Join(root, "incomplete", "job1")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "x.par2engine-ABC.tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := byName.check(ctx); !ok || filepath.Base(got) != "x.par2engine-ABC.tmp" {
		t.Fatalf("recursive name search = %q,%v", got, ok)
	}
	// The plain glob does not recurse, so the nested file is not a match for it.
	if _, ok, _ := pat.check(ctx); ok {
		t.Fatal("non-recursive glob matched a nested file")
	}
	if err := os.WriteFile(filepath.Join(root, "top.par2engine-Z.tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := pat.check(ctx); !ok || filepath.Base(got) != "top.par2engine-Z.tmp" {
		t.Fatalf("glob = %q,%v", got, ok)
	}
}

func TestPollUntil_ConditionBecomesTrue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.log")
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = os.WriteFile(path, []byte("ready\n"), 0o600) //nolint:errcheck // test helper goroutine
	}()
	cond := &logCondition{path: path, re: regexp.MustCompile(`ready`)}
	got, ok, err := pollUntil(t.Context(), cond, 5*time.Second, 5*time.Millisecond)
	if err != nil || !ok || got != "ready" {
		t.Fatalf("pollUntil = %q,%v,%v; want ready,true,nil", got, ok, err)
	}
}

func TestPollUntil_Timeout(t *testing.T) {
	cond := &logCondition{path: filepath.Join(t.TempDir(), "never.log"), re: regexp.MustCompile(`x`)}
	start := time.Now()
	got, ok, err := pollUntil(t.Context(), cond, 50*time.Millisecond, 5*time.Millisecond)
	if err != nil || ok || got != "(log not found)" {
		t.Fatalf("pollUntil = %q,%v,%v; want (log not found),false,nil", got, ok, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

type errCond struct{}

func (errCond) check(context.Context) (string, bool, error) { return "", false, errors.New("boom") }

func TestPollUntil_ReturnsConditionError(t *testing.T) {
	if _, _, err := pollUntil(t.Context(), errCond{}, time.Second, time.Millisecond); err == nil || err.Error() != "boom" {
		t.Fatalf("pollUntil err = %v, want boom", err)
	}
}

func TestCLI_Wait(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "g.log")
	appendFile(t, logPath, "hello world\n")
	dbPath := filepath.Join(dir, "h.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (n int); INSERT INTO t VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{"db ge holds", []string{"--db", dbPath, "--sql", "SELECT n FROM t", "--ge", "7"}, 0, "7\n", ""},
		{"db gt times out", []string{"--timeout", "50ms", "--interval", "5ms", "--db", dbPath, "--sql", "SELECT n FROM t", "--gt", "7"}, 1, "", "last observed: 7"},
		{"db eq holds", []string{"--db", dbPath, "--sql", "SELECT n FROM t", "--eq", "7"}, 0, "7\n", ""},
		{"db eq empty string holds", []string{"--db", dbPath, "--sql", "SELECT ''", "--eq", ""}, 0, "\n", ""},
		{"db eq empty is not satisfied by NULL", []string{"--timeout", "50ms", "--interval", "5ms", "--db", dbPath, "--sql", "SELECT NULL", "--eq", ""}, 1, "", "last observed: (NULL)"},
		{"db missing times out", []string{"--timeout", "50ms", "--interval", "5ms", "--db", filepath.Join(dir, "no.db"), "--sql", "SELECT 1", "--ge", "1"}, 1, "", "(db not found)"},
		{"db missing table keeps polling", []string{"--timeout", "50ms", "--interval", "5ms", "--db", dbPath, "--sql", "SELECT n FROM absent", "--ge", "1"}, 1, "", "no such table"},
		{"db syntax error exits at once", []string{"--timeout", "30s", "--db", dbPath, "--sql", "SELEC n FROM t", "--ge", "1"}, 2, "", "can never succeed"},
		{"log holds", []string{"--log", logPath, "--regex", "^hello"}, 0, "hello world\n", ""},
		{"log times out", []string{"--timeout", "50ms", "--interval", "5ms", "--log", logPath, "--regex", "nope"}, 1, "", "(no matching line)"},
		{"log from-end skips the stale line", []string{"--timeout", "50ms", "--interval", "5ms", "--log", logPath, "--regex", "^hello", "--from-end"}, 1, "", "(no matching line)"},
		{"glob holds", []string{"--glob", filepath.Join(dir, "*.log")}, 0, logPath + "\n", ""},
		{"glob-root holds", []string{"--glob-root", dir, "--glob-name", "*.db"}, 0, dbPath + "\n", ""},
		{"no condition", []string{}, 2, "", "exactly one of"},
		{"two conditions", []string{"--log", logPath, "--regex", "x", "--glob", "*"}, 2, "", "exactly one of"},
		{"db without comparison", []string{"--db", dbPath, "--sql", "SELECT 1"}, 2, "", "exactly one of --ge"},
		{"db with two comparisons", []string{"--db", dbPath, "--sql", "SELECT 1", "--ge", "1", "--eq", "1"}, 2, "", "exactly one of --ge"},
		{"non-numeric ge", []string{"--db", dbPath, "--sql", "SELECT 1", "--ge", "x"}, 2, "", "not a finite number"},
		{"NaN ge", []string{"--db", dbPath, "--sql", "SELECT 1", "--ge", "NaN"}, 2, "", "not a finite number"},
		{"infinite gt", []string{"--db", dbPath, "--sql", "SELECT 1", "--gt", "-Inf"}, 2, "", "not a finite number"},
		{"log without regex", []string{"--log", logPath}, 2, "", "--regex"},
		{"bad regex", []string{"--log", logPath, "--regex", "("}, 2, "", "--regex"},
		{"glob-root without name", []string{"--glob-root", dir}, 2, "", "--glob-name"},
		{"bad glob pattern", []string{"--glob", "["}, 2, "", "--glob"},
		{"zero interval", []string{"--interval", "0s", "--glob", "*"}, 2, "", "positive"},
		{"regex with glob", []string{"--glob", "*", "--regex", "x"}, 2, "", "--regex does not apply to --glob"},
		{"from-end with db", []string{"--db", dbPath, "--sql", "SELECT 1", "--ge", "1", "--from-end"}, 2, "", "--from-end does not apply to --db"},
		{"ge with log", []string{"--log", logPath, "--regex", "x", "--ge", "1"}, 2, "", "--ge does not apply to --log"},
		{"sql with glob-root", []string{"--glob-root", dir, "--glob-name", "*", "--sql", "SELECT 1"}, 2, "", "--sql does not apply to --glob-root"},
		{"glob-name with glob", []string{"--glob", "*", "--glob-name", "x"}, 2, "", "--glob-name does not apply to --glob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// A short default timeout keeps a broken "holds" case failing fast; a case's own
			// --timeout comes later in the args and wins.
			args := append([]string{"wait", "--timeout", "2s", "--interval", "10ms"}, tt.args...)
			code := run(context.Background(), args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, tt.wantCode, stderr.String())
			}
			if stdout.String() != tt.wantOut {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tt.wantOut)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}
