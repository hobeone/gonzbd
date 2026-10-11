package unpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/rarengine"
)

func extractLinkFixture(t *testing.T, name string, opts Options) (outDir string, res Result, logs string, err error) {
	t.Helper()
	outDir = t.TempDir()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	archive := Archive{
		Type:     RarArchive,
		Name:     name,
		MainFile: filepath.Join("testdata", name+".rar"),
	}
	res, err = GoUnRAR(context.Background(), log, archive, outDir, "", opts)
	return outDir, res, buf.String(), err
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestClassifyRarEngineError_DictionaryTooLarge(t *testing.T) {
	for _, err := range []error{
		rarengine.ErrDictionaryTooLarge,
		fmt.Errorf("go_unrar: write big.bin: %w", rarengine.ErrDictionaryTooLarge),
	} {
		got := ClassifyRarEngineError(err)
		if got != FailDictionaryTooLarge {
			t.Errorf("ClassifyRarEngineError(%v) = %v, want FailDictionaryTooLarge", err, got)
		}
		if got == FailCorrupt || got == FailUnknown {
			t.Errorf("dictionary limit classed as %v", got)
		}
	}
	if !strings.Contains(FailDictionaryTooLarge.String(), "dictionary") {
		t.Errorf("String() = %q does not name the dictionary", FailDictionaryTooLarge.String())
	}
}

func TestNoteDictionaryLimit(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	fh := &rarengine.FileHeader{Name: "big.bin", DictSize: 256 << 20}

	NoteDictionaryLimit(log, fmt.Errorf("x: %w", rarengine.ErrDictionaryTooLarge), fh)
	out := buf.String()
	for _, want := range []string{"32 MiB", "not corruption", "go_rar_fallback", "unrar is installed", "big.bin", "268435456"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}

	buf.Reset()
	NoteDictionaryLimit(log, rarengine.ErrCRCMismatch, fh)
	NoteDictionaryLimit(log, nil, fh)
	if buf.Len() != 0 {
		t.Errorf("unrelated error logged: %q", buf.String())
	}
}

// rarengine verifies a plain BLAKE2sp digest, so a damaged -htb archive is
// caught by the Go path as corruption (it extracted silently before).
func TestGoUnRAR_DamagedBlake2spArchiveIsCorrupt(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "damaged_b2",
		MainFile: filepath.Join("..", "..", "test", "fixtures", "par2", "layout_b", "damaged_b2.rar"),
	}
	res, err := GoUnRAR(context.Background(), slog.New(slog.DiscardHandler), archive, outDir, "", Options{})
	if err == nil {
		t.Fatal("damaged BLAKE2sp archive extracted without an error")
	}
	if !errors.Is(err, rarengine.ErrCRCMismatch) || res.Reason != FailCorrupt {
		t.Fatalf("err = %v, reason = %v; want ErrCRCMismatch and FailCorrupt", err, res.Reason)
	}
}
