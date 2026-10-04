package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The throwaway build cache. See the package doc's "# The build cache" section
// for why it exists and what it costs; this file is the mechanism.

// cacheBaseEnv names the directory a throwaway cache is created in. It is
// documented in the package doc and usage text.
const cacheBaseEnv = "MUTATE_GOCACHE_BASE"

// cacheDirEnv names a cache directory the caller owns and wants this run to
// use instead of creating its own; see adoptCache.
const cacheDirEnv = "MUTATE_GOCACHE_DIR"

// cacheDirPrefix is what every directory this command creates for a cache
// starts with, and what removeThrowaway requires before it deletes anything.
const cacheDirPrefix = "mutate-gocache-"

// throwaway is the cache directory this process created, if any. dir is
// non-empty from the moment startThrowaway creates the directory — before the
// seed — so an interrupt during seeding still reaches removeThrowaway.
//
// The mutex is separate from pending's: the signal handler restores the source
// file under pending's lock and then removes the cache, and the two have no
// reason to nest.
var throwaway struct {
	sync.Mutex
	dir  string
	base string
}

// cacheDir returns the GOCACHE the go subprocesses must run with, or "" when
// they should inherit the caller's environment unchanged.
func cacheDir() string {
	throwaway.Lock()
	defer throwaway.Unlock()
	return throwaway.dir
}

// goCommand builds a `go` invocation rooted at root, with GOCACHE pointed at
// the throwaway cache when there is one. goTest and listTests both go through
// it, so no go subprocess of this command reaches the shared cache around it.
func goCommand(ctx context.Context, root string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // G204: argv comes from the operator's own spec, the same trust level as testArgs
	cmd.Dir = root
	if dir := cacheDir(); dir != "" {
		// A duplicate key in cmd.Env resolves to the last one, so this wins
		// over a GOCACHE already in the environment.
		cmd.Env = append(os.Environ(), "GOCACHE="+dir)
	}
	return cmd
}

// sharedCacheDir asks the go tool where its cache is. It returns "" when
// there is none to seed from: GOCACHE=off, or a directory that does not exist
// yet (a cache somebody just deleted).
func sharedCacheDir(root string) string {
	cmd := exec.Command("go", "env", "GOCACHE")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" || dir == "off" {
		return ""
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// chooseCacheBase picks the directory the throwaway cache is created in.
//
// Hardlinks cannot cross filesystems, and a seed that cannot link would have
// to copy gigabytes, so the base has to be on the shared cache's filesystem.
// In order: MUTATE_GOCACHE_BASE, taken as written; $TMPDIR (or os.TempDir) if
// it is on the shared cache's filesystem; otherwise the shared cache's parent
// directory. With no shared cache there is nothing to link, and the temp area
// is as good as any.
func chooseCacheBase(shared string) string {
	if b := os.Getenv(cacheBaseEnv); b != "" {
		return b
	}
	tmp := os.TempDir()
	if shared == "" {
		return tmp
	}
	if sameFilesystem(tmp, shared) {
		return tmp
	}
	return filepath.Dir(shared)
}

func sameFilesystem(a, b string) bool {
	da, ok := deviceOf(a)
	if !ok {
		return false
	}
	db, ok := deviceOf(b)
	return ok && da == db
}

func deviceOf(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true //nolint:gosec,unconvert // a device number is only compared for equality; Dev is not uint64 on every platform
}

// startThrowaway creates this run's cache directory under base, registers it
// for removal, and seeds it from shared. A seed that fails or is partial is
// reported and tolerated: what the directory lacks is rebuilt, which costs
// time and nothing else.
//
// The directory is registered before it is seeded, so every exit path from
// there on removes it.
func startThrowaway(base, shared string) error {
	if err := os.MkdirAll(base, 0o700); err != nil { //nolint:gosec // G703: base is the operator's own MUTATE_GOCACHE_BASE or a directory derived from go env
		return fmt.Errorf("create the build-cache base %s: %w", base, err)
	}
	dir, err := os.MkdirTemp(base, cacheDirPrefix)
	if err != nil {
		return fmt.Errorf("create a throwaway build cache in %s: %w", base, err)
	}
	throwaway.Lock()
	throwaway.dir, throwaway.base = dir, filepath.Clean(base)
	throwaway.Unlock()

	if shared == "" {
		return nil
	}
	if err := seedCache(shared, dir); err != nil {
		fmt.Fprintf(os.Stderr, "mutate: seeding the throwaway build cache from %s failed (%v);\n"+
			"  continuing with what was seeded, so builds will be slower.\n"+
			"  Hardlinks need the same filesystem: set %s to a directory beside the Go build cache.\n",
			shared, err, cacheBaseEnv)
	}
	return nil
}

// adoptCache points the go subprocesses at a directory the caller owns, seeding
// it from shared only if it does not exist yet. It is how one throwaway cache
// serves a series of mutate runs — scripts/run_tests.sh gives each worker one,
// so the repository's own packages are built once per worker rather than once
// per spec — and the caller removes it. This command never does: throwaway.base
// stays empty, which removeThrowaway reads as "not ours to delete".
func adoptCache(dir, shared string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s must be an absolute path, got %q", cacheDirEnv, dir)
	}
	_, statErr := os.Stat(dir) //nolint:gosec // G703: dir is the operator's own MUTATE_GOCACHE_DIR
	fresh := errors.Is(statErr, fs.ErrNotExist)
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // G703: dir is the operator's own MUTATE_GOCACHE_DIR
		return fmt.Errorf("create %s: %w", dir, err)
	}
	throwaway.Lock()
	throwaway.dir, throwaway.base = dir, ""
	throwaway.Unlock()
	if fresh && shared != "" {
		if err := seedCache(shared, dir); err != nil {
			fmt.Fprintf(os.Stderr, "mutate: seeding %s from %s failed (%v); continuing with what was seeded\n", dir, shared, err)
		}
	}
	return nil
}

// removeThrowaway deletes the directory startThrowaway created, and only that
// one. It is idempotent, so every exit path may call it. A directory adopted
// with adoptCache is left alone.
//
// It refuses — returns an error and deletes nothing — unless the directory is
// a direct child of the base it was created in and carries cacheDirPrefix. The
// path is never read from the environment or a flag after creation, so the
// guard is a backstop against this file's own bugs rather than against input.
// os.RemoveAll unlinks hardlinks without touching the other name of the file,
// so the shared cache's entries survive it.
func removeThrowaway() error {
	throwaway.Lock()
	defer throwaway.Unlock()
	dir, base := throwaway.dir, throwaway.base
	if dir == "" || base == "" {
		return nil
	}
	if filepath.Dir(filepath.Clean(dir)) != base || !strings.HasPrefix(filepath.Base(dir), cacheDirPrefix) {
		return fmt.Errorf("refusing to remove %q: not a %s* directory directly under %q", dir, cacheDirPrefix, base)
	}

	// A killed `go` can leave a compiler still writing for a moment, which
	// makes RemoveAll race with a file appearing; a few retries cover it.
	var err error
	for range 5 {
		if err = os.RemoveAll(dir); err == nil {
			throwaway.dir, throwaway.base = "", ""
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// seedCache populates dst with the shared cache's entries without copying
// their bytes, so the throwaway cache starts warm and costs ~no disk.
//
// What each kind of entry gets, and why:
//
//   - `-d` output files are hard-linked. They are named by the hash of their
//     content, and go rewrites one in place only when the existing file is
//     already corrupt (DiskCache.copyFile in cmd/go/internal/cache).
//   - `-a` action entries are COPIED. go overwrites those in place when an
//     action is re-put with a different output — the cache's own comment in
//     putIndexEntry says it leaves them writable for that — and a hard-linked
//     one would let this run's builds rewrite the shared cache's entry.
//   - Executable entries (a `-d` directory, from `go run`/`go build -o`
//     outputs) have their files hard-linked into a new directory.
//   - trim.txt is written fresh, so go does not scan the throwaway cache for
//     entries to trim; it would only unlink our own names, but the scan costs.
//
// Everything else — go's own README, the fuzz directory — is not an entry and
// is skipped. A file that vanishes mid-seed (another go process trimmed it) is
// skipped: a missing entry is a cache miss. Any other failure stops the seed
// and is returned; the caller treats the result as a partial seed.
func seedCache(src, dst string) error {
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, max(4, runtime.NumCPU()))
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}

	for i := range 256 {
		sub := fmt.Sprintf("%02x", i)
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if failed() {
				return
			}
			if err := seedSubdir(filepath.Join(src, sub), filepath.Join(dst, sub)); err != nil {
				fail(err)
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	trim := strconv.FormatInt(time.Now().Unix(), 10)
	return os.WriteFile(filepath.Join(dst, "trim.txt"), []byte(trim), 0o666) //nolint:gosec // G306: matches the mode go gives trim.txt in a cache it creates
}

func seedSubdir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o777); err != nil { //nolint:gosec // G301: go creates its cache subdirectories 0777, subject to umask
		return err
	}
	for _, e := range entries {
		name := e.Name()
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		switch {
		case strings.HasSuffix(name, "-a") && !e.IsDir():
			b, err := os.ReadFile(from) //nolint:gosec // G304: from is an entry inside the Go build cache
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if err := os.WriteFile(to, b, 0o666); err != nil { //nolint:gosec // G306: matches the mode go gives action entries
				return err
			}
		case strings.HasSuffix(name, "-d") && e.IsDir():
			if err := linkTree(from, to); err != nil {
				return err
			}
		case strings.HasSuffix(name, "-d"):
			if err := os.Link(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// linkTree mirrors an executable cache entry: a directory of files.
func linkTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o777); err != nil { //nolint:gosec // G301: go creates executable entries 0777, subject to umask
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Link(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
