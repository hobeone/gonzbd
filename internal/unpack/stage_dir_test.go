package unpack

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/testutil"
)

func assertNoStageDir(t *testing.T, outDir string) {
	t.Helper()
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", outDir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), unpackStagePrefix) {
			t.Fatalf("leftover stage dir %s found in %s", e.Name(), outDir)
		}
	}
}

func TestStagedSubprocessExtraction(t *testing.T) {
	t.Parallel()

	t.Run("prepareStageDir and publishStagedExtraction direct", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		staleDir := filepath.Join(outDir, unpackStagePrefix+"stale")
		if err := os.MkdirAll(staleDir, 0o750); err != nil {
			t.Fatalf("mkdir staleDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(staleDir, "partial.mkv"), []byte("trunc"), 0o600); err != nil {
			t.Fatalf("write partial.mkv: %v", err)
		}

		stageDir, err := prepareStageDir(outDir)
		if err != nil {
			t.Fatalf("prepareStageDir: %v", err)
		}
		defer os.RemoveAll(stageDir)

		if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
			t.Errorf("staleDir still exists after prepareStageDir: err=%v", err)
		}

		if err := os.MkdirAll(filepath.Join(stageDir, "sub"), 0o750); err != nil {
			t.Fatalf("mkdir stageDir/sub: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stageDir, "movie.mkv"), []byte("full-payload"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stageDir, "sub", "en.srt"), []byte("subs"), 0o600); err != nil {
			t.Fatalf("write sub/en.srt: %v", err)
		}

		var logBuf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logBuf, nil))
		published, err := publishStagedExtraction(log, outDir, stageDir, Options{OverwriteFiles: false})
		if err != nil || len(published) != 2 {
			t.Fatalf("publishStagedExtraction = %v, %v; want 2 files", published, err)
		}

		// Second stage with OverwriteFiles=false preserves existing movie.mkv,
		// logs the skip, and emits an OnLine trace.
		if err := os.WriteFile(filepath.Join(stageDir, "movie.mkv"), []byte("overwritten"), 0o600); err != nil {
			t.Fatalf("write movie.mkv v2: %v", err)
		}
		logBuf.Reset()
		var lines []string
		published, err = publishStagedExtraction(log, outDir, stageDir, Options{
			OverwriteFiles: false,
			OnLine:         func(line string) { lines = append(lines, line) },
		})
		if err != nil || len(published) != 0 {
			t.Fatalf("publishStagedExtraction(overwrite=false) = %v, %v; want 0", published, err)
		}
		if !strings.Contains(logBuf.String(), "skipping existing file") {
			t.Errorf("expected 'skipping existing file' log entry, got %q", logBuf.String())
		}
		if !slices.Contains(lines, "Skipping existing: movie.mkv") {
			t.Errorf("OnLine lines = %v, want 'Skipping existing: movie.mkv'", lines)
		}
		got, err := os.ReadFile(filepath.Join(outDir, "movie.mkv"))
		if err != nil || string(got) != "full-payload" {
			t.Fatalf("movie.mkv after overwrite=false = %q, %v; want full-payload", got, err)
		}

		// OverwriteFiles=true replaces existing movie.mkv.
		published, err = publishStagedExtraction(log, outDir, stageDir, Options{OverwriteFiles: true})
		if err != nil || len(published) != 1 {
			t.Fatalf("publishStagedExtraction(overwrite=true) = %v, %v; want 1", published, err)
		}
		got, err = os.ReadFile(filepath.Join(outDir, "movie.mkv"))
		if err != nil || string(got) != "overwritten" {
			t.Fatalf("movie.mkv after overwrite=true = %q, %v; want overwritten", got, err)
		}

		CleanupStageDirs("")
	})

	t.Run("UnRAR interrupted extraction does not leave partial payload in outDir", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		binDir := t.TempDir()
		failScript := filepath.Join(binDir, "unrar-fail.sh")
		testutil.WriteExecutable(t, failScript, "#!/bin/sh\nfor last; do :; done\nprintf 'truncated' > \"${last}movie.mkv\"\nexit 1\n")
		okScript := filepath.Join(binDir, "unrar-ok.sh")
		testutil.WriteExecutable(t, okScript, "#!/bin/sh\nfor last; do :; done\nprintf 'complete-rar-payload' > \"${last}movie.mkv\"\nexit 0\n")

		archive := Archive{
			Type:     RarArchive,
			Name:     "movie",
			MainFile: filepath.Join(binDir, "movie.rar"),
			Parts:    []string{filepath.Join(binDir, "movie.rar")},
		}
		discard := slog.New(slog.DiscardHandler)

		// 1. Interrupted/failing extraction must not leave a truncated movie.mkv or stageDir in outDir.
		if _, err := UnRAR(t.Context(), discard, archive, outDir, "", Options{UnrarCommand: failScript, OverwriteFiles: false}); err == nil {
			t.Fatal("expected error from failing unrar script")
		}
		if _, err := os.Stat(filepath.Join(outDir, "movie.mkv")); !os.IsNotExist(err) {
			t.Fatalf("truncated movie.mkv leaked into outDir after failed UnRAR: err=%v", err)
		}
		assertNoStageDir(t, outDir)

		// 2. Subsequent rerun with OverwriteFiles=false extracts and publishes the full file.
		res, err := UnRAR(t.Context(), discard, archive, outDir, "", Options{UnrarCommand: okScript, OverwriteFiles: false})
		if err != nil || len(res.ExtractedFiles) != 1 {
			t.Fatalf("UnRAR rerun = %+v, %v; want 1 extracted file", res, err)
		}
		assertNoStageDir(t, outDir)
		got, err := os.ReadFile(filepath.Join(outDir, "movie.mkv"))
		if err != nil || string(got) != "complete-rar-payload" {
			t.Fatalf("movie.mkv = %q, %v; want complete-rar-payload", got, err)
		}
	})

	t.Run("SevenZip interrupted extraction does not leave partial payload in outDir", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		binDir := t.TempDir()
		failScript := filepath.Join(binDir, "7z-fail.sh")
		scriptBody := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in -o*) out=\"${a#-o}\" ;; esac; done\nprintf 'truncated' > \"$out/movie.mkv\"\nexit 2\n"
		testutil.WriteExecutable(t, failScript, scriptBody)
		okScript := filepath.Join(binDir, "7z-ok.sh")
		okBody := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in -o*) out=\"${a#-o}\" ;; esac; done\nprintf 'complete-7z-payload' > \"$out/movie.mkv\"\nexit 0\n"
		testutil.WriteExecutable(t, okScript, okBody)

		archive := Archive{
			Type:     SevenZipArchive,
			Name:     "movie",
			MainFile: filepath.Join(binDir, "movie.7z"),
			Parts:    []string{filepath.Join(binDir, "movie.7z")},
		}
		discard := slog.New(slog.DiscardHandler)

		if _, err := SevenZip(t.Context(), discard, archive, outDir, "", Options{SevenZipCommand: failScript, OverwriteFiles: false}); err == nil {
			t.Fatal("expected error from failing 7z script")
		}
		if _, err := os.Stat(filepath.Join(outDir, "movie.mkv")); !os.IsNotExist(err) {
			t.Fatalf("truncated movie.mkv leaked into outDir after failed SevenZip: err=%v", err)
		}
		assertNoStageDir(t, outDir)

		res, err := SevenZip(t.Context(), discard, archive, outDir, "", Options{SevenZipCommand: okScript, OverwriteFiles: false})
		if err != nil || len(res.ExtractedFiles) != 1 {
			t.Fatalf("SevenZip rerun = %+v, %v; want 1 extracted file", res, err)
		}
		assertNoStageDir(t, outDir)
		got, err := os.ReadFile(filepath.Join(outDir, "movie.mkv"))
		if err != nil || string(got) != "complete-7z-payload" {
			t.Fatalf("movie.mkv = %q, %v; want complete-7z-payload", got, err)
		}
	})
}

func TestExternalExtract_PasswordReachesSubprocess(t *testing.T) {
	t.Parallel()

	t.Run("unrar", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		binDir := t.TempDir()
		script := filepath.Join(binDir, "unrar-pw.sh")
		body := "#!/bin/sh\nsaw_pw=0\nsaw_skip=0\nfor a in \"$@\"; do\n  case \"$a\" in\n    -psecret) saw_pw=1 ;;\n    -p\"<redacted>\") exit 3 ;;\n    -o-) saw_skip=1 ;;\n  esac\n  last=\"$a\"\ndone\nif [ \"$saw_pw\" -ne 1 ] || [ \"$saw_skip\" -ne 1 ]; then exit 2; fi\nprintf 'decrypted-rar' > \"${last}secret.txt\"\nexit 0\n"
		testutil.WriteExecutable(t, script, body)
		archive := Archive{
			Type:     RarArchive,
			Name:     "secret",
			MainFile: filepath.Join(binDir, "secret.rar"),
			Parts:    []string{filepath.Join(binDir, "secret.rar")},
		}
		res, err := UnRAR(t.Context(), slog.New(slog.DiscardHandler), archive, outDir, "secret", Options{
			UnrarCommand:   script,
			OverwriteFiles: false,
		})
		if err != nil {
			t.Fatalf("UnRAR with password failed: %v", err)
		}
		if !strings.Contains(res.CommandLine, "-p<redacted>") || strings.Contains(res.CommandLine, "-psecret") {
			t.Errorf("CommandLine = %q, want -p<redacted> and no -psecret", res.CommandLine)
		}
		got, err := os.ReadFile(filepath.Join(outDir, "secret.txt"))
		if err != nil || string(got) != "decrypted-rar" {
			t.Fatalf("secret.txt = %q, %v; want decrypted-rar", got, err)
		}
	})

	t.Run("7z", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		binDir := t.TempDir()
		script := filepath.Join(binDir, "7z-pw.sh")
		body := "#!/bin/sh\nsaw_pw=0\nsaw_skip=0\nfor a in \"$@\"; do\n  case \"$a\" in\n    -psecret) saw_pw=1 ;;\n    -p\"<redacted>\") exit 3 ;;\n    -aos) saw_skip=1 ;;\n    -o*) out=\"${a#-o}\" ;;\n  esac\ndone\nif [ \"$saw_pw\" -ne 1 ] || [ \"$saw_skip\" -ne 1 ]; then exit 2; fi\nprintf 'decrypted-7z' > \"$out/secret.txt\"\nexit 0\n"
		testutil.WriteExecutable(t, script, body)
		archive := Archive{
			Type:     SevenZipArchive,
			Name:     "secret",
			MainFile: filepath.Join(binDir, "secret.7z"),
			Parts:    []string{filepath.Join(binDir, "secret.7z")},
		}
		res, err := SevenZip(t.Context(), slog.New(slog.DiscardHandler), archive, outDir, "secret", Options{
			SevenZipCommand: script,
			OverwriteFiles:  false,
		})
		if err != nil {
			t.Fatalf("SevenZip with password failed: %v", err)
		}
		if !strings.Contains(res.CommandLine, "-p<redacted>") || strings.Contains(res.CommandLine, "-psecret") {
			t.Errorf("CommandLine = %q, want -p<redacted> and no -psecret", res.CommandLine)
		}
		got, err := os.ReadFile(filepath.Join(outDir, "secret.txt"))
		if err != nil || string(got) != "decrypted-7z" {
			t.Fatalf("secret.txt = %q, %v; want decrypted-7z", got, err)
		}
	})
}

// lockedStageDirs are the directories makeLockedStage creates, each holding a
// file f, with the mode each is left at, deepest first so a directory is
// locked only after its children were made. They are what an unrar run
// without -ai leaves for an archive that stores such modes (#862): ro has no
// owner write, locked and locked/deeper have no owner permission at all, and
// noread has owner write and search but no read.
var lockedStageDirs = []struct {
	rel  string
	mode os.FileMode
}{
	{filepath.Join("locked", "deeper"), 0o000},
	{"locked", 0o000},
	{"noread", 0o300},
	{"ro", 0o555},
}

// lockedStagePublished is what publishing makeLockedStage's stage yields,
// in walk order.
var lockedStagePublished = []string{
	filepath.Join("locked", "deeper", "f"),
	filepath.Join("locked", "f"),
	filepath.Join("noread", "f"),
	filepath.Join("ro", "f"),
}

// makeLockedStage creates a staging directory in outDir holding
// lockedStageDirs. Its cleanup gives every directory under outDir its owner
// bits back, visiting each before reading it, so the temp directory can be
// removed whatever the code under test did.
func makeLockedStage(t *testing.T, outDir string) string {
	t.Helper()
	stageDir, err := prepareStageDir(outDir)
	if err != nil {
		t.Fatalf("prepareStageDir: %v", err)
	}
	t.Cleanup(func() {
		err := filepath.WalkDir(outDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || !d.IsDir() {
				return walkErr
			}
			return os.Chmod(path, 0o700)
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("restore stage modes: %v", err)
		}
	})
	for _, d := range lockedStageDirs {
		dir := filepath.Join(stageDir, d.rel)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d.rel, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("payload"), 0o600); err != nil {
			t.Fatalf("write %s/f: %v", d.rel, err)
		}
	}
	for _, d := range lockedStageDirs {
		if err := os.Chmod(filepath.Join(stageDir, d.rel), d.mode); err != nil {
			t.Fatalf("chmod %s: %v", d.rel, err)
		}
	}
	return stageDir
}

func TestPublishStagedExtraction_ReadOnlyStagedDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	outDir := t.TempDir()
	stageDir := makeLockedStage(t, outDir)
	// open has owner rwx already, so ownStageDirs leaves it at 0o711. With
	// no group read, a copy of that mode cannot be masked into 0o750 by any
	// umask, as 0o755 or 0o777 would be by 0o027.
	open := filepath.Join(stageDir, "open")
	if err := os.Mkdir(open, 0o750); err != nil {
		t.Fatalf("mkdir open: %v", err)
	}
	if err := os.Chmod(open, 0o711); err != nil {
		t.Fatalf("chmod open: %v", err)
	}

	published, err := publishStagedExtraction(slog.New(slog.DiscardHandler), outDir, stageDir, Options{})
	if err != nil {
		t.Fatalf("publishStagedExtraction: %v", err)
	}
	if !slices.Equal(published, lockedStagePublished) {
		t.Errorf("published = %v, want %v", published, lockedStagePublished)
	}
	for _, rel := range lockedStagePublished {
		if got, err := os.ReadFile(filepath.Join(outDir, rel)); err != nil || string(got) != "payload" {
			t.Errorf("%s = %q, %v; want payload", rel, got, err)
		}
	}

	// A published directory gets 0o750, not the archive's mode. Comparing
	// with a directory made with 0o750 applies the process umask to both.
	ref := filepath.Join(t.TempDir(), "ref")
	if err := os.Mkdir(ref, 0o750); err != nil {
		t.Fatalf("mkdir ref: %v", err)
	}
	refInfo, err := os.Stat(ref)
	if err != nil {
		t.Fatalf("stat ref: %v", err)
	}
	for _, dir := range []string{"locked", "noread", "ro", "open"} {
		info, err := os.Stat(filepath.Join(outDir, dir))
		if err != nil {
			t.Fatalf("stat published %s: %v", dir, err)
		}
		if info.Mode().Perm() != refInfo.Mode().Perm() {
			t.Errorf("published %s mode = %v, want %v", dir, info.Mode().Perm(), refInfo.Mode().Perm())
		}
	}

	if err := os.RemoveAll(stageDir); err != nil {
		t.Fatalf("RemoveAll(stage) after publish: %v", err)
	}
}

func TestRemoveStage_ReadOnlyStagedDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}

	t.Run("removeStageDir", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		stageDir := makeLockedStage(t, outDir)
		var logBuf bytes.Buffer
		removeStageDir(slog.New(slog.NewTextHandler(&logBuf, nil)), stageDir)
		assertNoStageDir(t, outDir)
		if logBuf.Len() != 0 {
			t.Errorf("removeStageDir logged %q, want nothing", logBuf.String())
		}
	})

	t.Run("CleanupStageDirs", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		makeLockedStage(t, outDir)
		CleanupStageDirs(outDir)
		assertNoStageDir(t, outDir)
	})
}

// ExtractStaged publishes what extract wrote only when extract succeeds, and
// removes the stage either way. With OneFolder and without OverwriteFiles a
// name outDir already holds is published under a uniquePath name; without
// OneFolder it is skipped, as the external extractors' publish does.
func TestExtractStaged(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	write := func(name, body string) func(string) error {
		return func(stageDir string) error {
			return os.WriteFile(filepath.Join(stageDir, name), []byte(body), 0o600)
		}
	}

	t.Run("a failed extraction publishes nothing", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		errExtract := errors.New("member two failed")
		published, err := ExtractStaged(log, outDir, Options{}, func(stageDir string) error {
			if err := write("one.mkv", "member one")(stageDir); err != nil {
				return err
			}
			return errExtract
		})
		if !errors.Is(err, errExtract) || published != nil {
			t.Fatalf("ExtractStaged = %v, %v; want nil, %v", published, err, errExtract)
		}
		if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
			t.Errorf("outDir holds %v after a failed extraction, want nothing", entries)
		}
	})

	t.Run("a successful extraction is published", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		published, err := ExtractStaged(log, outDir, Options{}, write("one.mkv", "member one"))
		if err != nil || !slices.Equal(published, []string{"one.mkv"}) {
			t.Fatalf("ExtractStaged = %v, %v; want [one.mkv]", published, err)
		}
		if b, err := os.ReadFile(filepath.Join(outDir, "one.mkv")); err != nil || string(b) != "member one" {
			t.Errorf("one.mkv = %q, %v", b, err)
		}
		assertNoStageDir(t, outDir)
	})

	for _, tc := range []struct {
		name      string
		opts      Options
		published []string
		taken     string
	}{
		{"flat renames a taken name", Options{OneFolder: true}, []string{"one_1.mkv"}, "earlier"},
		{"not flat skips a taken name", Options{}, nil, "earlier"},
		{"overwrite replaces a taken name", Options{OneFolder: true, OverwriteFiles: true}, []string{"one.mkv"}, "member one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(outDir, "one.mkv"), []byte("earlier"), 0o600); err != nil {
				t.Fatal(err)
			}
			published, err := ExtractStaged(log, outDir, tc.opts, write("one.mkv", "member one"))
			if err != nil || !slices.Equal(published, tc.published) {
				t.Fatalf("ExtractStaged = %v, %v; want %v", published, err, tc.published)
			}
			if b, err := os.ReadFile(filepath.Join(outDir, "one.mkv")); err != nil || string(b) != tc.taken {
				t.Errorf("one.mkv = %q, %v; want %q", b, err, tc.taken)
			}
			assertNoStageDir(t, outDir)
		})
	}

	t.Run("a stage that cannot be made is reported", func(t *testing.T) {
		t.Parallel()
		called := false
		_, err := ExtractStaged(log, filepath.Join(t.TempDir(), "missing"), Options{}, func(string) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("ExtractStaged into a missing dir = %v, extract called %v; want an error and no call", err, called)
		}
	})
}

func TestMkdirAllTracked(t *testing.T) {
	t.Parallel()
	// MkdirAll creates x, then fails on the over-long name: x is recorded
	// although the call errors.
	tooLong := strings.Repeat("a", 300)
	for _, tc := range []struct {
		name    string
		before  []string // paths under the root; a trailing slash is a directory
		known   []string
		rel     string
		want    []string // created, shallowest first
		wantErr bool
		// wantAbsent: rel does not exist on disk afterwards.
		wantAbsent bool
	}{
		{name: "every level missing", rel: "a/b/c", want: []string{"a", "a/b", "a/b/c"}},
		{name: "only the missing levels", before: []string{"a/", "a/b/"}, rel: "a/b/c/d", want: []string{"a/b/c", "a/b/c/d"}},
		{name: "an existing directory records nothing", before: []string{"a/"}, rel: "a"},
		// The directory is absent on disk, and a known one is not made again.
		{name: "a known directory is left alone", known: []string{"a/b"}, rel: "a/b", wantAbsent: true},
		{name: "a file where a directory is needed", before: []string{"f"}, rel: "f/a", wantErr: true},
		{name: "levels made before a failure are recorded", rel: "x/" + tooLong, want: []string{"x", "x/" + tooLong}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			populate(t, dir, tc.before)
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			known := map[string]struct{}{}
			for _, k := range tc.known {
				known[k] = struct{}{}
			}
			var created []string
			err = mkdirAllTracked(root, tc.rel, known, &created)
			if (err != nil) != tc.wantErr {
				t.Fatalf("mkdirAllTracked(%q) error = %v, wantErr %v", tc.rel, err, tc.wantErr)
			}
			if !slices.Equal(created, tc.want) {
				t.Errorf("created = %v, want %v", created, tc.want)
			}
			if _, statErr := os.Lstat(filepath.Join(dir, tc.rel)); !tc.wantErr && tc.wantAbsent != errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("Lstat(%q) = %v; want absent %v", tc.rel, statErr, tc.wantAbsent)
			}
			if err == nil {
				// A repeat call for the directory just ensured records nothing.
				var more []string
				if err := mkdirAllTracked(root, tc.rel, known, &more); err != nil || len(more) != 0 {
					t.Errorf("second call = %v, %v; want no error and nothing recorded", more, err)
				}
			}
		})
	}
}

// TestUndoPublish pins that a removal that fails is reported and does not stop
// the removals after it: the created directory d holds a file this publish did
// not write, so it stays and is returned in the error, while the published
// file and the empty created directory e are still removed.
func TestUndoPublish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	populate(t, dir, []string{"d/", "e/", "p.txt", "d/foreign"})
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	// created is walked last to first, so d is attempted before e.
	err = undoPublish(root, dir, []string{"p.txt"}, []string{"e", "d"})
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != "d" {
		t.Fatalf("undoPublish error = %v; want a PathError for the directory d that could not be removed", err)
	}
	if got, want := dirTree(t, dir), []string{"d", "d/foreign"}; !slices.Equal(got, want) {
		t.Errorf("dir after undoPublish = %v, want %v", got, want)
	}
}

// TestExtractStaged_APartialPublishLeavesNothing pins that a publish that
// fails after renaming some members undoes them: it removes the files it
// published and the directories it created, and keeps a directory that
// existed before it.
func TestExtractStaged_APartialPublishLeavesNothing(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name   string
		before []string // paths under outDir; a trailing slash is a directory
		stage  []string // files written to the stage
		after  []string // outDir once the publish has failed
	}{
		{
			name:   "a blocked top-level member leaves the earlier one unpublished",
			before: []string{"b.txt/", "b.txt/x"},
			stage:  []string{"a.txt", "b.txt"},
			after:  []string{"b.txt", "b.txt/x"},
		},
		{
			name:   "a directory the publish created is removed",
			before: []string{"z.txt/", "z.txt/x"},
			stage:  []string{"new/a.txt", "z.txt"},
			after:  []string{"z.txt", "z.txt/x"},
		},
		{
			name:   "a directory that existed before stays with its contents",
			before: []string{"old/", "old/keep.txt", "z.txt/", "z.txt/x"},
			stage:  []string{"old/a.txt", "z.txt"},
			after:  []string{"old", "old/keep.txt", "z.txt", "z.txt/x"},
		},
		{
			name:   "a file where an archive directory is needed fails the set",
			before: []string{"d"},
			stage:  []string{"d/a.txt"},
			after:  []string{"d"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outDir := t.TempDir()
			populate(t, outDir, tc.before)
			published, err := ExtractStaged(log, outDir, Options{OverwriteFiles: true}, func(stageDir string) error {
				for _, p := range tc.stage {
					full := filepath.Join(stageDir, p)
					if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
						return err
					}
					if err := os.WriteFile(full, []byte("staged"), 0o600); err != nil {
						return err
					}
				}
				return nil
			})
			if err == nil || published != nil {
				t.Fatalf("ExtractStaged = %v, %v; want nil and an error", published, err)
			}
			if got := dirTree(t, outDir); !slices.Equal(got, tc.after) {
				t.Errorf("outDir after the failed publish = %v, want %v", got, tc.after)
			}
		})
	}
}
