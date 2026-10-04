package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// resetThrowaway clears the package-level cache registration around a test.
func resetThrowaway(t *testing.T) {
	t.Helper()
	throwaway.Lock()
	throwaway.dir, throwaway.base = "", ""
	throwaway.Unlock()
	t.Cleanup(func() {
		throwaway.Lock()
		throwaway.dir, throwaway.base = "", ""
		throwaway.Unlock()
	})
}

func writeTree(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeSharedCache lays out the pieces of a Go build cache that seedCache
// distinguishes: an action entry, an output file, an executable entry, and
// the files it must not carry over.
func fakeSharedCache(t *testing.T) string {
	t.Helper()
	shared := t.TempDir()
	writeTree(t, filepath.Join(shared, "ab", "abcd-a"), "v1 action\n")
	writeTree(t, filepath.Join(shared, "ab", "abcd-d"), "object bytes")
	writeTree(t, filepath.Join(shared, "cd", "cdef-d", "tool"), "executable bytes")
	writeTree(t, filepath.Join(shared, "README"), "go cache\n")
	writeTree(t, filepath.Join(shared, "fuzz", "corpus"), "x")
	writeTree(t, filepath.Join(shared, "trim.txt"), "1")
	return shared
}

func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(fa, fb)
}

func TestStartThrowaway_SeedsByLinkAndCopy(t *testing.T) {
	resetThrowaway(t)
	shared := fakeSharedCache(t)
	base := t.TempDir()

	if err := startThrowaway(base, shared); err != nil {
		t.Fatal(err)
	}
	dir := cacheDir()
	if filepath.Dir(dir) != base || !strings.HasPrefix(filepath.Base(dir), cacheDirPrefix) {
		t.Fatalf("cache dir = %q, want a %s* directory directly under %q", dir, cacheDirPrefix, base)
	}

	// The output file is linked: its bytes cost no disk.
	if !sameInode(t, filepath.Join(shared, "ab", "abcd-d"), filepath.Join(dir, "ab", "abcd-d")) {
		t.Error("the -d output was copied, not hard-linked")
	}
	if !sameInode(t, filepath.Join(shared, "cd", "cdef-d", "tool"), filepath.Join(dir, "cd", "cdef-d", "tool")) {
		t.Error("the executable entry's file was copied, not hard-linked")
	}

	// The action entry is copied: go rewrites those in place, so a link would
	// let this run edit the shared cache's entry.
	copied := filepath.Join(dir, "ab", "abcd-a")
	if sameInode(t, filepath.Join(shared, "ab", "abcd-a"), copied) {
		t.Fatal("the -a action entry is a hardlink; a rewrite in the throwaway cache would modify the shared one")
	}
	got, err := os.ReadFile(copied) //nolint:gosec // G304: path is under this test's temp dir
	if err != nil || string(got) != "v1 action\n" {
		t.Errorf("copied action entry = %q, %v; want its bytes carried over", got, err)
	}

	// Rewriting the copy in place — what putIndexEntry does — leaves the
	// shared entry alone.
	if err := os.WriteFile(copied, []byte("rewritten"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(shared, "ab", "abcd-a")); string(b) != "v1 action\n" { //nolint:gosec // G304: test temp dir
		t.Errorf("shared action entry = %q after the throwaway copy was rewritten", b)
	}

	if _, err := os.Stat(filepath.Join(dir, "README")); err == nil {
		t.Error("README was carried over; only cache entries are seeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "fuzz")); err == nil {
		t.Error("the fuzz directory was carried over")
	}
	trim, err := os.ReadFile(filepath.Join(dir, "trim.txt")) //nolint:gosec // G304: test temp dir
	if err != nil || string(trim) == "1" || string(trim) == "" {
		t.Errorf("trim.txt = %q, %v; want a fresh timestamp so go does not scan-trim the copy", trim, err)
	}
}

func TestRemoveThrowaway_LeavesTheSharedEntriesAlone(t *testing.T) {
	resetThrowaway(t)
	shared := fakeSharedCache(t)
	base := t.TempDir()
	if err := startThrowaway(base, shared); err != nil {
		t.Fatal(err)
	}
	dir := cacheDir()

	if err := removeThrowaway(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the throwaway cache still exists after removal: %v", err)
	}
	for _, p := range []string{"ab/abcd-a", "ab/abcd-d", "cd/cdef-d/tool"} {
		if _, err := os.Stat(filepath.Join(shared, p)); err != nil {
			t.Errorf("shared entry %s is gone after the throwaway cache was removed: %v", p, err)
		}
	}
	if err := removeThrowaway(); err != nil {
		t.Errorf("a second removal = %v, want nil (idempotent)", err)
	}
	if cacheDir() != "" {
		t.Errorf("cacheDir = %q after removal, want empty so no later go command is pointed at it", cacheDir())
	}
}

func TestAdoptCache_SeedsOnlyAFreshDirectoryAndNeverRemovesIt(t *testing.T) {
	resetThrowaway(t)
	shared := fakeSharedCache(t)
	dir := filepath.Join(t.TempDir(), "worker-cache")

	if err := adoptCache(dir, shared); err != nil {
		t.Fatal(err)
	}
	if cacheDir() != dir {
		t.Fatalf("cacheDir = %q, want the adopted %q", cacheDir(), dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "ab", "abcd-d")); err != nil {
		t.Errorf("a fresh adopted directory was not seeded: %v", err)
	}

	// A second adoption of the same directory is a later spec in the same
	// worker: what the first run left there must survive, and the shared
	// cache's entries must not be re-seeded over it.
	writeTree(t, filepath.Join(dir, "ff", "leftover-d"), "from the first spec")
	if err := os.Remove(filepath.Join(dir, "ab", "abcd-d")); err != nil {
		t.Fatal(err)
	}
	if err := adoptCache(dir, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ff", "leftover-d")); err != nil {
		t.Errorf("an entry from the previous run was lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ab", "abcd-d")); err == nil {
		t.Error("an existing adopted directory was seeded again")
	}

	if err := removeThrowaway(); err != nil {
		t.Errorf("removeThrowaway = %v, want nil: an adopted directory is not ours", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("an adopted directory was removed: %v", err)
	}

	if err := adoptCache("relative/dir", shared); err == nil {
		t.Error("a relative directory was accepted")
	}
}

func TestRemoveThrowaway_RefusesADirectoryItDidNotCreate(t *testing.T) {
	resetThrowaway(t)
	base := t.TempDir()
	victim := filepath.Join(base, "precious")
	writeTree(t, filepath.Join(victim, "file"), "keep")

	// Not the cache prefix, and a base that is not the parent: each alone
	// must refuse.
	for name, reg := range map[string][2]string{
		"wrong prefix": {victim, base},
		"wrong parent": {filepath.Join(base, cacheDirPrefix+"x"), t.TempDir()},
		"base itself":  {base, base},
	} {
		throwaway.Lock()
		throwaway.dir, throwaway.base = reg[0], reg[1]
		throwaway.Unlock()
		if err := removeThrowaway(); err == nil {
			t.Errorf("%s: removeThrowaway = nil, want a refusal", name)
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "file")); err != nil {
		t.Errorf("a directory the command did not create was touched: %v", err)
	}
}

func TestGoCommand_PassesTheThrowawayCacheAsGOCACHE(t *testing.T) {
	resetThrowaway(t)
	t.Setenv("GOCACHE", "/inherited/cache")

	if cmd := goCommand(context.Background(), ".", []string{"version"}); cmd.Env != nil {
		t.Errorf("with no throwaway cache Env = %v, want nil (inherit)", cmd.Env)
	}

	throwaway.Lock()
	throwaway.dir = "/throwaway"
	throwaway.Unlock()
	cmd := goCommand(context.Background(), ".", []string{"version"})
	var gocache []string
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "GOCACHE=") {
			gocache = append(gocache, e)
		}
	}
	// exec resolves duplicate keys to the last, so the last must be ours.
	if len(gocache) == 0 || gocache[len(gocache)-1] != "GOCACHE=/throwaway" {
		t.Errorf("GOCACHE entries = %v, want the last to be GOCACHE=/throwaway", gocache)
	}
}

func TestChooseCacheBase(t *testing.T) {
	shared := t.TempDir()
	t.Setenv(cacheBaseEnv, "/explicit")
	if got := chooseCacheBase(shared); got != "/explicit" {
		t.Errorf("with %s set, base = %q, want it taken as written", cacheBaseEnv, got)
	}
	t.Setenv(cacheBaseEnv, "")
	t.Setenv("TMPDIR", t.TempDir())
	if got := chooseCacheBase(shared); got != os.Getenv("TMPDIR") {
		t.Errorf("base = %q, want $TMPDIR when it shares the shared cache's filesystem", got)
	}
	if got := chooseCacheBase(""); got != os.Getenv("TMPDIR") {
		t.Errorf("with no shared cache base = %q, want $TMPDIR", got)
	}
}

// The remaining tests drive real exit paths in a child process, since they
// end in os.Exit or a signal. The child is this test binary, selected by an
// environment variable, as restore_exit_test.go does.
const (
	childScenarioEnv = "MUTATE_TEST_CACHE_SCENARIO"
	childRootEnv     = "MUTATE_TEST_CACHE_ROOT"
	childSpecEnv     = "MUTATE_TEST_CACHE_SPEC"
)

func TestThrowawayChild(t *testing.T) {
	scenario := os.Getenv(childScenarioEnv)
	if scenario == "" {
		t.Skip("child half of the throwaway-cache exit-path tests")
	}
	base := os.Getenv(cacheBaseEnv)
	switch scenario {
	case "exit":
		mustStart(base)
		exit(0)
	case "fatal":
		mustStart(base)
		fatal("simulated failure")
	case "signal":
		installSignalRestore()
		mustStart(base)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(30 * time.Second)
	case "adopt":
		if err := adoptCache(os.Getenv(cacheDirEnv), ""); err != nil {
			fatal("child: %v", err)
		}
		exit(0)
	case "sharedcache":
		runSpec(os.Getenv(childRootEnv), os.Getenv(childSpecEnv), false, false, true)
	case "panic":
		// run's first write goes through writeFile; panicking there is a panic
		// in the middle of runSpec, after the cache exists.
		writeFile = func(string, []byte, os.FileMode) error { panic("simulated panic") }
		runSpec(os.Getenv(childRootEnv), os.Getenv(childSpecEnv), false, false, false)
	default:
		runSpec(os.Getenv(childRootEnv), os.Getenv(childSpecEnv), false, false, false)
	}
}

func mustStart(base string) {
	if err := startThrowaway(base, ""); err != nil {
		fatal("child: %v", err)
	}
}

// runChild runs TestThrowawayChild for a scenario and returns its exit code
// and combined output. The shared GOCACHE is the one given, never the
// caller's.
func runChild(t *testing.T, scenario, base, gocache, root, spec string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestThrowawayChild$") //nolint:gosec // G204: re-exec of the test binary itself
	cmd.Env = append(os.Environ(),
		childScenarioEnv+"="+scenario,
		cacheBaseEnv+"="+base,
		"GOCACHE="+gocache,
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
		childRootEnv+"="+root,
		childSpecEnv+"="+spec,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	ee, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("run child: %v", err)
	}
	return ee.ExitCode(), string(out)
}

func leftovers(t *testing.T, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestThrowawayCache_IsRemovedOnEveryNonSpecExitPath(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		wantCode int
	}{
		{"exit", 0},
		{"fatal", 2},
		{"signal", 128 + int(syscall.SIGTERM)},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			base := t.TempDir()
			code, out := runChild(t, tc.scenario, base, t.TempDir(), "", "")
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d\n%s", code, tc.wantCode, out)
			}
			if left := leftovers(t, base); len(left) != 0 {
				t.Errorf("the throwaway cache survived the %s path: %v", tc.scenario, left)
			}
		})
	}
}

func TestThrowawayCache_AdoptedDirectorySurvivesExit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker-cache")
	t.Setenv(cacheDirEnv, dir)
	code, out := runChild(t, "adopt", t.TempDir(), t.TempDir(), "", "")
	if code != 0 {
		t.Fatalf("exit code = %d\n%s", code, out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the caller's cache directory was removed on exit: %v", err)
	}
}

// tinyModule writes a one-package module whose test pins F, and a spec whose
// single mutation is the given replacement. It returns the module root and
// the spec path.
func tinyModule(t *testing.T, replace, run string) (root, spec string) {
	t.Helper()
	root = t.TempDir()
	writeTree(t, filepath.Join(root, "go.mod"), "module example.test/tiny\n\ngo 1.21\n")
	writeTree(t, filepath.Join(root, "p.go"), "package tiny\n\nfunc F() int { return 1 }\n")
	writeTree(t, filepath.Join(root, "p_test.go"),
		"package tiny\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tif F() != 1 {\n\t\tt.Fatal(\"F() != 1\")\n\t}\n}\n")
	spec = filepath.Join(root, "tiny.spec")
	body := "pkg ./\n"
	if run != "" {
		body += "run " + run + "\n"
	}
	body += "\n[the mutation]\nfile p.go\n--- anchor\nreturn 1\n--- replace\n" + replace + "\n--- end\n"
	writeTree(t, spec, body)
	return root, spec
}

// cacheFiles lists every file under dir, so growth is a set difference
// rather than a size that a concurrent go process could also move.
func cacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	return files
}

// A fresh GOCACHE per test, warmed once with the unmutated package: what the
// child must not do is add the MUTATED build to it.
func warmedCache(t *testing.T, root string) string {
	t.Helper()
	gocache := t.TempDir()
	cmd := exec.Command("go", "test", "-count=1", "./...") //nolint:gosec // G204: fixed argv
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOCACHE="+gocache, "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the tiny module here: %v\n%s", err, out)
	}
	return gocache
}

func TestThrowawayCache_KeepsMutatedBuildsOutOfTheSharedCache(t *testing.T) {
	for _, tc := range []struct {
		name, scenario, replace, want string
		wantCode                      int
		wantGrowth                    bool
	}{
		{"killed", "spec", "return 2", "KILLED", 0, false},
		{"survived", "spec", "return 1 + 0", "SURVIVED", 1, false},
		{"compile error", "spec", `return "x"`, "COMPILE_ERROR", 1, false},
		// The control: the opt-out must leak, or the zero growth above says
		// nothing about where the builds went.
		{"opt-out leaks", "sharedcache", "return 3", "KILLED", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, spec := tinyModule(t, tc.replace, "")
			gocache := warmedCache(t, root)
			base := t.TempDir()
			before := cacheFiles(t, gocache)

			code, out := runChild(t, tc.scenario, base, gocache, root, spec)
			if code != tc.wantCode || !strings.Contains(out, tc.want) {
				t.Fatalf("exit code %d, want %d with a %s verdict:\n%s", code, tc.wantCode, tc.want, out)
			}

			after := cacheFiles(t, gocache)
			grew := len(after) > len(before)
			if grew != tc.wantGrowth {
				t.Errorf("shared cache files %d -> %d (grew=%v), want grew=%v:\n%s",
					len(before), len(after), grew, tc.wantGrowth, out)
			}
			if left := leftovers(t, base); len(left) != 0 {
				t.Errorf("the throwaway cache survived: %v", left)
			}
			// The source file must be back, as before.
			if b, _ := os.ReadFile(filepath.Join(root, "p.go")); !strings.Contains(string(b), "return 1") { //nolint:gosec // G304: test temp dir
				t.Errorf("p.go was left mutated: %q", b)
			}
		})
	}
}

func TestThrowawayCache_IsRemovedWhenRunSpecPanics(t *testing.T) {
	root, spec := tinyModule(t, "return 2", "")
	gocache := warmedCache(t, root)
	base := t.TempDir()

	code, out := runChild(t, "panic", base, gocache, root, spec)
	if code == 0 || !strings.Contains(out, "simulated panic") {
		t.Fatalf("exit code %d; want the child to die of the simulated panic:\n%s", code, out)
	}
	if left := leftovers(t, base); len(left) != 0 {
		t.Errorf("the throwaway cache survived a panic: %v", left)
	}
}
