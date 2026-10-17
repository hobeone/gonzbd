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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// waitCondition is one pollable predicate. check reports the value observed on this poll
// (what a timeout prints) and whether the condition holds. A transient problem such as a
// missing file or table is an observation, not an error, so a condition that is not ready yet
// keeps polling.
type waitCondition interface {
	check(ctx context.Context) (observed string, ok bool)
}

// pollUntil polls cond every interval until it holds, the timeout elapses, or ctx is done.
// It returns the last observed value; matched reports whether the condition held.
func pollUntil(ctx context.Context, cond waitCondition, timeout, interval time.Duration) (observed string, matched bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		prev := observed
		observed, matched = cond.check(ctx)
		if matched {
			return observed, true
		}
		if ctx.Err() != nil && prev != "" {
			// The check raced the deadline; its error would mask the last real observation.
			return prev, false
		}
		select {
		case <-ctx.Done():
			return observed, false
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

func (c *sqlCondition) check(ctx context.Context) (string, bool) {
	if _, err := os.Stat(c.path); err != nil {
		return "(db not found)", false
	}
	dsn := (&url.URL{Scheme: "file", Opaque: filepath.ToSlash(c.path), RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "(open error: " + err.Error() + ")", false
	}
	defer func() { _ = db.Close() }() //nolint:errcheck // best-effort close of a read-only handle

	var raw any
	if err := db.QueryRowContext(ctx, c.query).Scan(&raw); err != nil {
		return "(query error: " + err.Error() + ")", false
	}
	got := ""
	switch v := raw.(type) {
	case nil:
	case []byte:
		got = string(v)
	default:
		got = fmt.Sprint(v)
	}
	return got, compareScalar(got, c.op, c.want)
}

// compareScalar applies op between got and want: "eq" is a string compare; "ge" and "gt" are
// numeric and fail when either side is not a number.
func compareScalar(got, op, want string) bool {
	if op == "eq" {
		return got == want
	}
	g, err := strconv.ParseFloat(got, 64)
	if err != nil {
		return false
	}
	w, err := strconv.ParseFloat(want, 64)
	if err != nil {
		return false
	}
	if op == "gt" {
		return g > w
	}
	return g >= w
}

// logCondition holds once a complete line of the file matches. It remembers its read offset so
// each poll reads only the bytes appended since the last one.
type logCondition struct {
	path    string
	re      *regexp.Regexp
	offset  int64
	pending []byte
}

func (c *logCondition) check(context.Context) (string, bool) {
	f, err := os.Open(c.path) //nolint:gosec // operator-supplied log path
	if err != nil {
		return "(log not found)", false
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close of a read handle

	if fi, err := f.Stat(); err == nil && fi.Size() < c.offset {
		c.offset, c.pending = 0, nil // truncated or rotated: start over
	}
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return "(seek error: " + err.Error() + ")", false
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return "(read error: " + err.Error() + ")", false
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
			return line, true
		}
	}
	return "(no matching line)", false
}

// globCondition holds once the pattern matches a path: either a filepath.Glob pattern, or a
// recursive search under root for a base name matching name.
type globCondition struct {
	pattern    string
	root, name string
}

func (c *globCondition) check(context.Context) (string, bool) {
	if c.pattern != "" {
		matches, err := filepath.Glob(c.pattern)
		if err != nil || len(matches) == 0 {
			return "(no match)", false
		}
		return matches[0], true
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
		return "(no match)", false
	}
	return found, true
}

func runWait(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var (
		timeout  = flags.Duration("timeout", 10*time.Minute, "give up after this long (exit 1)")
		interval = flags.Duration("interval", 200*time.Millisecond, "poll interval")
		dbPath   = flags.String("db", "", "SQLite file to query (with --sql and one of --ge, --gt, --eq)")
		query    = flags.String("sql", "", "query returning one scalar")
		ge       = flags.String("ge", "", "hold when the scalar is numerically >= this")
		gt       = flags.String("gt", "", "hold when the scalar is numerically > this")
		eq       = flags.String("eq", "", "hold when the scalar equals this string")
		logPath  = flags.String("log", "", "log file to watch (with --regex)")
		regex    = flags.String("regex", "", "regular expression a log line must match")
		glob     = flags.String("glob", "", "filepath.Glob pattern that must match at least one path")
		globRoot = flags.String("glob-root", "", "directory to search recursively (with --glob-name)")
		globName = flags.String("glob-name", "", "filepath.Match pattern for the base name under --glob-root")
	)
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cond, err := buildWaitCondition(flags, *dbPath, *query, *ge, *gt, *eq, *logPath, *regex, *glob, *globRoot, *globName)
	if err != nil {
		printMsg(stderr, "error: %v\n", err)
		return 2
	}
	if *interval <= 0 || *timeout <= 0 {
		printMsgln(stderr, "error: --timeout and --interval must be positive")
		return 2
	}

	observed, matched := pollUntil(ctx, cond, *timeout, *interval)
	if !matched {
		printMsg(stderr, "timeout after %s; last observed: %s\n", *timeout, observed)
		return 1
	}
	printMsgln(stdout, observed)
	return 0
}

// buildWaitCondition checks that exactly one condition family was selected and that its
// companion flags are present and well-formed.
func buildWaitCondition(flags *flag.FlagSet, dbPath, query, ge, gt, eq, logPath, regex, glob, globRoot, globName string) (waitCondition, error) { //nolint:ireturn // the three conditions share only the waitCondition interface
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })

	families := 0
	for _, on := range []bool{dbPath != "", logPath != "", glob != "", globRoot != ""} {
		if on {
			families++
		}
	}
	if families != 1 {
		return nil, errors.New("exactly one of --db, --log, --glob, --glob-root is required")
	}

	switch {
	case dbPath != "":
		ops := 0
		op, want := "", ""
		for _, c := range []struct{ name, op, val string }{{"ge", "ge", ge}, {"gt", "gt", gt}, {"eq", "eq", eq}} {
			if set[c.name] {
				ops++
				op, want = c.op, c.val
			}
		}
		if query == "" || ops != 1 {
			return nil, errors.New("--db needs --sql and exactly one of --ge, --gt, --eq")
		}
		if op != "eq" {
			if _, err := strconv.ParseFloat(want, 64); err != nil {
				return nil, fmt.Errorf("--%s %q is not a number", op, want)
			}
		}
		return &sqlCondition{path: dbPath, query: query, op: op, want: want}, nil
	case logPath != "":
		if regex == "" {
			return nil, errors.New("--log needs --regex")
		}
		re, err := regexp.Compile(regex)
		if err != nil {
			return nil, fmt.Errorf("--regex: %w", err)
		}
		return &logCondition{path: logPath, re: re}, nil
	case glob != "":
		if _, err := filepath.Match(glob, ""); err != nil {
			return nil, fmt.Errorf("--glob: %w", err)
		}
		return &globCondition{pattern: glob}, nil
	default:
		if globName == "" {
			return nil, errors.New("--glob-root needs --glob-name")
		}
		if _, err := filepath.Match(globName, ""); err != nil {
			return nil, fmt.Errorf("--glob-name: %w", err)
		}
		return &globCondition{root: globRoot, name: globName}, nil
	}
}
