// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// SQLite primary result codes that mean "not now" rather than "this will never work".
const (
	sqliteBusy     = 5
	sqliteLocked   = 6
	sqliteCantOpen = 14
)

// waitCondition is one pollable predicate. check reports the value observed on this poll
// (what a timeout prints) and whether the condition holds. A transient problem such as a
// missing file or table is an observation, not an error, so a condition that is not ready yet
// keeps polling. A non-nil error means polling can never succeed and stops the wait.
type waitCondition interface {
	check(ctx context.Context) (observed string, ok bool, err error)
}

// pollUntil polls cond every interval until it holds, the timeout elapses, or ctx is done.
// It returns the last observed value; matched reports whether the condition held. A check error
// ends the wait at once.
func pollUntil(ctx context.Context, cond waitCondition, timeout, interval time.Duration) (observed string, matched bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	seen := false
	for {
		prev := observed
		observed, matched, err = cond.check(ctx)
		if err != nil {
			return observed, false, err
		}
		if matched {
			return observed, true, nil
		}
		if ctx.Err() != nil && seen {
			// The check raced the deadline; its error would mask the last real observation.
			return prev, false, nil
		}
		seen = true
		select {
		case <-ctx.Done():
			return observed, false, nil
		case <-tick.C:
		}
	}
}

// sqlCondition holds when a one-scalar query against a SQLite file satisfies the comparison.
type sqlCondition struct {
	path, query string
	op          string // "ge", "gt" or "eq"
	want        string
}

func (c *sqlCondition) check(ctx context.Context) (observed string, ok bool, err error) {
	if _, err := os.Stat(c.path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "(db not found)", false, nil
		}
		return "", false, fmt.Errorf("stat %s: %w", c.path, err)
	}
	dsn := (&url.URL{Scheme: "file", Opaque: filepath.ToSlash(c.path), RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", false, fmt.Errorf("open %s: %w", c.path, err)
	}
	defer func() { _ = db.Close() }() //nolint:errcheck // best-effort close of a read-only handle

	var raw any
	if err := db.QueryRowContext(ctx, c.query).Scan(&raw); err != nil {
		if retryableQueryError(ctx, err) {
			return "(query error: " + err.Error() + ")", false, nil
		}
		return "", false, fmt.Errorf("query %q on %s can never succeed: %w", c.query, c.path, err)
	}
	var got string
	switch v := raw.(type) {
	case nil:
		return "(NULL)", false, nil
	case []byte:
		got = string(v)
	default:
		got = fmt.Sprint(v)
	}
	return got, compareScalar(got, c.op, c.want), nil
}

// retryableQueryError reports whether a failed query may succeed on a later poll: the deadline
// racing the query, a database that is busy, locked or not openable yet, a table not created yet,
// or no row yet. Anything else, a syntax error or a wrong column count for example, will fail
// the same way every time.
func retryableQueryError(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, sql.ErrNoRows) {
		return true
	}
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqliteBusy, sqliteLocked, sqliteCantOpen:
		return true
	}
	return strings.Contains(se.Error(), "no such table")
}

// parseFinite parses s as a number, rejecting NaN and the infinities.
func parseFinite(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// compareScalar applies op between got and want: "eq" is a string compare; "ge" and "gt" are
// numeric and fail when either side is not a finite number.
func compareScalar(got, op, want string) bool {
	if op == "eq" {
		return got == want
	}
	g, ok := parseFinite(got)
	if !ok {
		return false
	}
	w, ok := parseFinite(want)
	if !ok {
		return false
	}
	if op == "gt" {
		return g > w
	}
	return g >= w
}

// logCondition holds once a complete line of the file matches. It remembers its read offset so
// each poll reads only the bytes appended since the last one; a nonzero starting offset skips
// what the file held before the wait began.
type logCondition struct {
	path    string
	re      *regexp.Regexp
	offset  int64
	pending []byte
}

func (c *logCondition) check(context.Context) (observed string, ok bool, err error) {
	f, err := os.Open(c.path) //nolint:gosec // operator-supplied log path
	if err != nil {
		return "(log not found)", false, nil
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close of a read handle

	if fi, err := f.Stat(); err == nil && fi.Size() < c.offset {
		c.offset, c.pending = 0, nil // truncated or rotated: start over
	}
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return "(seek error: " + err.Error() + ")", false, nil
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return "(read error: " + err.Error() + ")", false, nil
	}
	c.offset += int64(len(chunk))
	c.pending = append(c.pending, chunk...)
	for {
		i := bytes.IndexByte(c.pending, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(c.pending[:i], "\r"))
		c.pending = c.pending[i+1:]
		if c.re.MatchString(line) {
			return line, true, nil
		}
	}
	return "(no matching line)", false, nil
}

// globCondition holds once the pattern matches a path: either a filepath.Glob pattern, or a
// recursive search under root for a base name matching name.
type globCondition struct {
	pattern    string
	root, name string
}

func (c *globCondition) check(context.Context) (observed string, ok bool, err error) {
	if c.pattern != "" {
		matches, err := filepath.Glob(c.pattern)
		if err != nil {
			return "", false, fmt.Errorf("glob %q: %w", c.pattern, err)
		}
		if len(matches) == 0 {
			return "(no match)", false, nil
		}
		return matches[0], true, nil
	}
	var found string
	_ = filepath.WalkDir(c.root, func(path string, d fs.DirEntry, walkErr error) error { //nolint:errcheck // a missing or unreadable root just means not yet
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return fs.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if ok, _ := filepath.Match(c.name, d.Name()); ok { //nolint:errcheck // pattern validated at flag parse
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if found == "" {
		return "(no match)", false, nil
	}
	return found, true, nil
}

// waitFlags are the values of the condition flags `wait` accepts, and which of them were
// given on the command line (a flag set to its zero value is still given).
type waitFlags struct {
	db, sql, ge, gt, eq      string
	log, regex               string
	fromEnd                  bool
	glob, globRoot, globName string
	set                      map[string]bool
	timeout, interval        time.Duration
}

func (w *waitFlags) register(flags *flag.FlagSet) {
	flags.DurationVar(&w.timeout, "timeout", 10*time.Minute, "give up after this long (exit 1)")
	flags.DurationVar(&w.interval, "interval", 200*time.Millisecond, "poll interval")
	flags.StringVar(&w.db, "db", "", "SQLite file to query (with --sql and one of --ge, --gt, --eq)")
	flags.StringVar(&w.sql, "sql", "", "query returning one scalar")
	flags.StringVar(&w.ge, "ge", "", "hold when the scalar is numerically >= this")
	flags.StringVar(&w.gt, "gt", "", "hold when the scalar is numerically > this")
	flags.StringVar(&w.eq, "eq", "", "hold when the scalar equals this string (NULL never equals anything)")
	flags.StringVar(&w.log, "log", "", "log file to watch (with --regex)")
	flags.StringVar(&w.regex, "regex", "", "regular expression a log line must match")
	flags.BoolVar(&w.fromEnd, "from-end", false, "with --log, ignore lines already in the file when the wait starts")
	flags.StringVar(&w.glob, "glob", "", "filepath.Glob pattern that must match at least one path")
	flags.StringVar(&w.globRoot, "glob-root", "", "directory to search recursively (with --glob-name)")
	flags.StringVar(&w.globName, "glob-name", "", "filepath.Match pattern for the base name under --glob-root")
}

func runWait(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var w waitFlags
	w.register(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	w.set = map[string]bool{}
	flags.Visit(func(f *flag.Flag) { w.set[f.Name] = true })

	cond, err := w.condition()
	if err != nil {
		printMsg(stderr, "error: %v\n", err)
		return 2
	}
	if w.interval <= 0 || w.timeout <= 0 {
		printMsgln(stderr, "error: --timeout and --interval must be positive")
		return 2
	}

	observed, matched, err := pollUntil(ctx, cond, w.timeout, w.interval)
	if err != nil {
		printMsg(stderr, "error: %v\n", err)
		return 2
	}
	if !matched {
		printMsg(stderr, "timeout after %s; last observed: %s\n", w.timeout, observed)
		return 1
	}
	printMsgln(stdout, observed)
	return 0
}

// condition checks that exactly one condition family was selected, that no flag belonging to
// another family was given, and that the family's own flags are present and well-formed.
func (w *waitFlags) condition() (waitCondition, error) { //nolint:ireturn // the three conditions share only the waitCondition interface
	families := []struct {
		flag  string   // the flag that selects the family
		owned []string // flags only that family accepts
	}{
		{"db", []string{"sql", "ge", "gt", "eq"}},
		{"log", []string{"regex", "from-end"}},
		{"glob", nil},
		{"glob-root", []string{"glob-name"}},
	}
	var chosen []string
	for _, fam := range families {
		if w.value(fam.flag) != "" {
			chosen = append(chosen, fam.flag)
		}
	}
	if len(chosen) != 1 {
		return nil, errors.New("exactly one of --db, --log, --glob, --glob-root is required")
	}
	family := chosen[0]
	for _, fam := range families {
		if fam.flag == family {
			continue
		}
		for _, name := range fam.owned {
			if w.set[name] {
				return nil, fmt.Errorf("--%s does not apply to --%s", name, family)
			}
		}
	}

	switch family {
	case "db":
		return w.sqlCondition()
	case "log":
		if w.regex == "" {
			return nil, errors.New("--log needs --regex")
		}
		re, err := regexp.Compile(w.regex)
		if err != nil {
			return nil, fmt.Errorf("--regex: %w", err)
		}
		c := &logCondition{path: w.log, re: re}
		if w.fromEnd {
			if fi, err := os.Stat(w.log); err == nil {
				c.offset = fi.Size()
			}
		}
		return c, nil
	case "glob":
		if _, err := filepath.Match(w.glob, ""); err != nil {
			return nil, fmt.Errorf("--glob: %w", err)
		}
		return &globCondition{pattern: w.glob}, nil
	default:
		if w.globName == "" {
			return nil, errors.New("--glob-root needs --glob-name")
		}
		if _, err := filepath.Match(w.globName, ""); err != nil {
			return nil, fmt.Errorf("--glob-name: %w", err)
		}
		return &globCondition{root: w.globRoot, name: w.globName}, nil
	}
}

func (w *waitFlags) value(family string) string {
	switch family {
	case "db":
		return w.db
	case "log":
		return w.log
	case "glob":
		return w.glob
	default:
		return w.globRoot
	}
}

func (w *waitFlags) sqlCondition() (waitCondition, error) { //nolint:ireturn // callers want the interface
	ops := 0
	op, want := "", ""
	for _, c := range []struct{ name, val string }{{"ge", w.ge}, {"gt", w.gt}, {"eq", w.eq}} {
		if w.set[c.name] {
			ops++
			op, want = c.name, c.val
		}
	}
	if w.sql == "" || ops != 1 {
		return nil, errors.New("--db needs --sql and exactly one of --ge, --gt, --eq")
	}
	if op != "eq" {
		if _, ok := parseFinite(want); !ok {
			return nil, fmt.Errorf("--%s %q is not a finite number", op, want)
		}
	}
	return &sqlCondition{path: w.db, query: w.sql, op: op, want: want}, nil
}
