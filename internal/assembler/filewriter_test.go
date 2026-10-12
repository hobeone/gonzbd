package assembler

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

type fileWriterOpt func(*fileWriter)

// withWriteError makes every WriteAt fail with err, injected on the writeAt
// field before first use — the same override shape diskProbe.statfs uses.
func withWriteError(err error) fileWriterOpt {
	return func(w *fileWriter) {
		w.writeAt = func([]byte, int64) (int, error) { return 0, err }
	}
}

func newTestFileWriter(t *testing.T, opts ...fileWriterOpt) *fileWriter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target.dat")
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	t.Cleanup(func() { _ = fh.Close() })
	w := newFileWriter(fh, path, fileKey{jobID: "job1", fileIdx: 0})
	for _, o := range opts {
		o(w)
	}
	return w
}

// TestFileWriter_AcceptNotesTheArticleForSync pins that a successful write puts
// the article in unsynced, which is what a failed sync rolls back.
func TestFileWriter_AcceptNotesTheArticleForSync(t *testing.T) {
	w := newTestFileWriter(t)

	if err := w.accept(articleID{msgID: "a5", artIdx: 5}, 4096, bytes.Repeat([]byte{1}, 100)); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(w.unsynced, []int32{5}) {
		t.Fatalf("unsynced = %v, want [5] — a failed Sync could not roll back an article it was never told about", w.unsynced)
	}
}

// TestFileWriter_DirectWriteFailureIsNotNoted pins that an article whose
// WriteAt failed is absent from unsynced and the fault comes back classified.
func TestFileWriter_DirectWriteFailureIsNotNoted(t *testing.T) {
	w := newTestFileWriter(t, withWriteError(syscall.EIO))

	err := w.accept(articleID{msgID: "a1", artIdx: 1}, 0, bytes.Repeat([]byte{7}, 64))
	if err == nil {
		t.Fatal("Accept returned nil error after EIO")
	}
	if _, ok := errors.AsType[*storagefault.Fault](err); !ok {
		t.Fatalf("Accept error = %T, want *storagefault.Fault", err)
	}
	if got := w.unsynced; len(got) != 0 {
		t.Fatalf("unsynced = %v after a failed direct write, want empty", got)
	}
}

// TestAssembler_HasNoAckSurface pins X2 from the assembler's side.
func TestAssembler_HasNoAckSurface(t *testing.T) {
	ot := reflect.TypeFor[Options]()
	forbidden := []string{
		"MarkArticlesDone", "MarkArticlesDoneByIdx", "MarkArticlesFailed",
		"MarkArticlesFailedByIdx", "SetFileExtents",
	}
	for field := range ot.Fields() {
		if slices.Contains(forbidden, field.Name) {
			t.Errorf("Options.%s still exists — the assembler must have no ack authority", field.Name)
		}
	}
}

// TestNoSymbolNamesWriteAtDurable guards the naming trap: a state named durable
// that means only "reached WriteAt" is the conflation that
// docs/durability-contract.md § "The state of an article" separates (Done is
// a write, not an fsync; `complete=1` is the fsync), and
// leaving one would teach the next reader the bug back.
//
// This test earned its place immediately — the symbol survived the first pass
// of the cutover, in a const block between two deleted functions, and this is
// what found it. A report had already claimed it was gone.
//
// It walks the working tree with grep rather than asking git, because git grep
// sees only TRACKED files: a reintroduction in a file not yet added would pass
// a git-based check and then land with the commit that adds it.
//
// Scoped to Go sources across the whole repository. Repo-wide because the name
// must not reappear in any package; Go-only because docs/ legitimately discuss
// the old symbol when explaining why it was removed, and a test that fails on
// prose describing history is noise that trains its own suppression.
func TestNoSymbolNamesWriteAtDurable(t *testing.T) {
	// The needle is assembled at run time so this file does not contain the
	// literal and therefore cannot match itself. A self-listing grep test
	// fails forever for a reason that has nothing to do with the code it
	// guards, and the usual repair is to delete the guard.
	needle := "outcome" + "Durable"
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	out, err := exec.Command("grep", "-rn", "--include=*.go", needle, root).CombinedOutput()
	if err == nil && len(out) > 0 {
		t.Errorf("%s still present; a not-durable state must not be named durable:\n%s", needle, out)
	}
}

// repoRoot returns the module root, so the grep above covers every package
// rather than only the one the test happens to run in.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// TestFileWriter_WriteOneFailureTouchesNoAccounting pins that a failed write
// leaves the admission exactly as it found it: the roll-back is resolve's, at
// the one call site that holds the outcome, and a writeOne that applied half
// of it on its own would be a second owner of the part count.
func TestFileWriter_WriteOneFailureTouchesNoAccounting(t *testing.T) {
	w := newTestFileWriter(t, withWriteError(syscall.EIO))
	w.admit(9)
	before := stateOf(w, 9)

	if err := w.writeOne(articleID{msgID: "a9", artIdx: 9}, 0, []byte("xy")); err == nil {
		t.Fatal("writeOne returned nil after EIO")
	}
	if got := stateOf(w, 9); got != before {
		t.Errorf("state after a failed writeOne = %+v, want %+v unchanged", got, before)
	}
	if got := w.unsynced; len(got) != 0 {
		t.Errorf("unsynced = %v after a failed write, want empty", got)
	}
}
