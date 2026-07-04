package postproc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestVerifiedSets_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vs := NewVerifiedSets(dir, nil)
	vs.MarkVerified("movie", true)
	vs.MarkVerified("subs", false)

	if !vs.IsVerified("movie") {
		t.Error("movie should be verified")
	}
	if vs.IsVerified("subs") {
		t.Error("subs should NOT be verified (marked false)")
	}
	if vs.IsVerified("unknown") {
		t.Error("unknown set should not be verified")
	}

	// Reload from disk.
	vs2 := NewVerifiedSets(dir, nil)
	if !vs2.IsVerified("movie") {
		t.Error("after reload: movie should be verified")
	}
	if vs2.IsVerified("subs") {
		t.Error("after reload: subs should NOT be verified")
	}
}

func TestVerifiedSets_AllVerified(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Empty → false.
	vs := NewVerifiedSets(dir, nil)
	if vs.AllVerified() {
		t.Error("empty set should not be AllVerified")
	}

	// Partial → false.
	vs.MarkVerified("a", true)
	vs.MarkVerified("b", false)
	if vs.AllVerified() {
		t.Error("partial set should not be AllVerified")
	}

	// All true → true.
	vs.MarkVerified("b", true)
	if !vs.AllVerified() {
		t.Error("all-true set should be AllVerified")
	}
}

func TestVerifiedSets_Persistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vs := NewVerifiedSets(dir, nil)
	vs.MarkVerified("set1", true)
	vs.MarkVerified("set2", true)

	// Create a new instance pointing to the same directory.
	vs2 := NewVerifiedSets(dir, nil)
	if !vs2.IsVerified("set1") {
		t.Error("set1 should be verified after reload")
	}
	if !vs2.IsVerified("set2") {
		t.Error("set2 should be verified after reload")
	}
	if !vs2.AllVerified() {
		t.Error("all sets should be verified after reload")
	}
}

func TestVerifiedSets_FileLocation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vs := NewVerifiedSets(dir, nil)
	vs.MarkVerified("test", true)

	// Verify the file is in the expected location.
	expectedPath := filepath.Join(dir, constants.JobAdminDirName, constants.VerifiedFileName)
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("verified file not found at expected path %s: %v", expectedPath, err)
	}
}

func TestVerifiedSets_CorruptFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write garbage to the verified file location.
	adminDir := filepath.Join(dir, constants.JobAdminDirName)
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(adminDir, constants.VerifiedFileName)
	if err := os.WriteFile(path, []byte("not json"), 0o640); err != nil {
		t.Fatal(err)
	}

	// Should recover gracefully with empty state.
	vs := NewVerifiedSets(dir, nil)
	if vs.AllVerified() {
		t.Error("corrupt file should result in empty (not AllVerified) state")
	}
	if vs.IsVerified("anything") {
		t.Error("corrupt file should result in no verified sets")
	}
}

func TestVerifiedSets_LoadSave_Direct(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vs := NewVerifiedSets(dir, nil)
	vs.sets["direct_test"] = true

	if err := vs.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	vs2 := &VerifiedSets{
		sets: make(map[string]bool),
		path: vs.path,
	}
	vs2.load()
	if !vs2.sets["direct_test"] {
		t.Error("expected load to populate direct_test")
	}
}

func TestVerifiedSets_LogOnSaveFailure(t *testing.T) {
	dir := t.TempDir()

	// 1. Success case: save should succeed, and no warning should be logged.
	vs := NewVerifiedSets(dir, nil)

	oldLogger := slog.Default()
	handler := &testLogHandler{}
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	vs.MarkVerified("movie", true)

	handler.mu.Lock()
	warnLoggedSuccess := handler.warnLogged
	handler.warnLogged = false
	handler.mu.Unlock()

	if warnLoggedSuccess {
		t.Error("expected no warning logs on successful MarkVerified")
	}

	// 2. Failure case: make path a directory so writing fails, and warning should be logged.
	vsFail := NewVerifiedSets(dir, nil)
	vsFail.path = filepath.Join(dir, "is-a-directory")
	if err := os.Mkdir(vsFail.path, 0o755); err != nil {
		t.Fatal(err)
	}

	vsFail.MarkVerified("movie", true)

	handler.mu.Lock()
	warnLoggedFailure := handler.warnLogged
	handler.mu.Unlock()

	if !warnLoggedFailure {
		t.Error("expected warning log on failed MarkVerified save")
	}
}

func TestVerified_CorruptedState(t *testing.T) {
	dir := t.TempDir()

	adminDir := filepath.Join(dir, constants.JobAdminDirName)
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(adminDir, constants.VerifiedFileName)
	if err := os.WriteFile(path, []byte("{corrupted json"), 0o640); err != nil {
		t.Fatal(err)
	}

	oldLogger := slog.Default()
	handler := &testLogHandler{}
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	vs := NewVerifiedSets(dir, nil)
	if vs.AllVerified() {
		t.Error("corrupt file should result in empty (not AllVerified) state")
	}
	if vs.IsVerified("anything") {
		t.Error("corrupt file should result in no verified sets")
	}

	handler.mu.Lock()
	warnLogged := handler.warnLogged
	handler.mu.Unlock()

	if !warnLogged {
		t.Error("expected warning log when loading corrupted verification state")
	}
}

type testLogRecorder struct {
	attrs   []slog.Attr
	records *[]slog.Record
}

func (h *testLogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *testLogRecorder) Handle(_ context.Context, r slog.Record) error {
	for _, a := range h.attrs {
		r.AddAttrs(a)
	}
	*h.records = append(*h.records, r)
	return nil
}
func (h *testLogRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &testLogRecorder{attrs: newAttrs, records: h.records}
}
func (h *testLogRecorder) WithGroup(string) slog.Handler { return h }

func TestVerifiedSets_ComponentScopedLogging(t *testing.T) {
	oldLogger := slog.Default()
	records := &[]slog.Record{}
	slog.SetDefault(slog.New(&testLogRecorder{records: records}))
	defer slog.SetDefault(oldLogger)

	dir := t.TempDir()
	adminDir := filepath.Join(dir, constants.JobAdminDirName)
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(adminDir, constants.VerifiedFileName)
	if err := os.WriteFile(path, []byte("{corrupted json"), 0o640); err != nil {
		t.Fatal(err)
	}

	_ = NewVerifiedSets(dir, nil)

	if len(*records) == 0 {
		t.Fatal("expected at least one log record for corrupted state, got 0")
	}

	foundComponent := false
	for _, rec := range *records {
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "component" && a.Value.String() == "postproc" {
				foundComponent = true
				return false
			}
			return true
		})
	}
	if !foundComponent {
		t.Errorf("expected warning log record to have attribute component=postproc; records: %v", *records)
	}
}
