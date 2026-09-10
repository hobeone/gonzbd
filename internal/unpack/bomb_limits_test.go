package unpack

import (
	"errors"
	"strings"
	"testing"
)

// errTestBomb stands in for each format's own bomb sentinel. The helpers under
// test take it as a parameter precisely so the caller's wording survives, so a
// local sentinel exercises them exactly as go_tar and go_sevenzip do.
var errTestBomb = errors.New("archive bomb")

// TestBombLimitDefaults pins which zero values are replaced and which are not.
//
// The asymmetry is the point: maxSize and maxRatio treat any non-positive value
// as unset, but minThreshold replaces only an exact 0. A NEGATIVE minThreshold
// is meaningful — projectedBombCheck reads it as "apply the ratio ceiling with
// no lower bound at all" — so folding it into the default would silently
// disable the caller's request.
func TestBombLimitDefaults(t *testing.T) {
	t.Parallel()

	const (
		defMaxSize   = 250 * 1024 * 1024 * 1024
		defMaxRatio  = 250
		defThreshold = 10 * 1024 * 1024
	)

	tests := []struct {
		name                            string
		opts                            Options
		wantSize, wantRatio, wantThresh int64
	}{
		{
			name:     "an empty Options takes every default",
			opts:     Options{},
			wantSize: defMaxSize, wantRatio: defMaxRatio, wantThresh: defThreshold,
		},
		{
			name:     "explicit values are preserved",
			opts:     Options{MaxSize: 4096, MaxRatio: 3, MinBombThreshold: 512},
			wantSize: 4096, wantRatio: 3, wantThresh: 512,
		},
		{
			name:     "negative size and ratio are treated as unset",
			opts:     Options{MaxSize: -1, MaxRatio: -1},
			wantSize: defMaxSize, wantRatio: defMaxRatio, wantThresh: defThreshold,
		},
		{
			name:     "a negative threshold is preserved, not defaulted",
			opts:     Options{MinBombThreshold: -1},
			wantSize: defMaxSize, wantRatio: defMaxRatio, wantThresh: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			size, ratio, thresh := bombLimitDefaults(tt.opts)
			if size != tt.wantSize {
				t.Errorf("maxSize = %d, want %d", size, tt.wantSize)
			}
			if ratio != tt.wantRatio {
				t.Errorf("maxRatio = %d, want %d", ratio, tt.wantRatio)
			}
			if thresh != tt.wantThresh {
				t.Errorf("minThreshold = %d, want %d", thresh, tt.wantThresh)
			}
		})
	}
}

// TestProjectedBombCheck covers the three ways it can refuse an entry and the
// two ways the ratio ceiling is deliberately not applied.
//
// It is a projection: it decides from the entry's CLAIMED size before any of
// its bytes are read, which is what lets a tar header's lie be refused without
// first extracting the lie.
func TestProjectedBombCheck(t *testing.T) {
	t.Parallel()

	check := func(entrySize uint64, arcSize, total int64, opts Options) error {
		running := total
		return projectedBombCheck(entrySize, arcSize, &running, opts,
			"entry.bin", "declared", errTestBomb, "gotar")
	}

	t.Run("a negative running total is refused as corrupt state", func(t *testing.T) {
		t.Parallel()
		err := check(1, 0, -1, Options{})
		if err == nil {
			t.Fatal("a negative totalRead was accepted")
		}
		if errors.Is(err, errTestBomb) {
			t.Error("a negative total is invalid accounting, not a bomb; " +
				"reporting it as one attributes our own bug to the archive")
		}
		if !strings.Contains(err.Error(), "negative total read count") {
			t.Errorf("error = %v, want it to name the negative count", err)
		}
	})

	t.Run("the absolute ceiling counts the running total, not just this entry", func(t *testing.T) {
		t.Parallel()
		// Neither 600 alone nor the 600 already read exceeds 1000, but their
		// sum does — projecting is the whole point.
		err := check(600, 0, 600, Options{MaxSize: 1000})
		if !errors.Is(err, errTestBomb) {
			t.Fatalf("err = %v, want the bomb sentinel", err)
		}
		if !strings.Contains(err.Error(), "declared") {
			t.Errorf("error = %v, want it to carry the caller's sizeLabel", err)
		}
	})

	t.Run("the ratio ceiling refuses an over-expanding archive", func(t *testing.T) {
		t.Parallel()
		// 5000 from a 100-byte archive is 50x, over a limit of 4.
		err := check(5000, 100, 0, Options{MaxSize: 1 << 40, MaxRatio: 4, MinBombThreshold: 1})
		if !errors.Is(err, errTestBomb) {
			t.Fatalf("err = %v, want the bomb sentinel", err)
		}
		if !strings.Contains(err.Error(), "ratio") {
			t.Errorf("error = %v, want it to name the ratio limit", err)
		}
	})

	t.Run("the ratio ceiling is not applied below the threshold", func(t *testing.T) {
		t.Parallel()
		// Same 50x expansion, but only 5000 bytes — small archives routinely
		// expand hugely and refusing them would reject ordinary files.
		if err := check(5000, 100, 0, Options{MaxSize: 1 << 40, MaxRatio: 4, MinBombThreshold: 1 << 20}); err != nil {
			t.Errorf("err = %v, want nil below the minimum bomb threshold", err)
		}
	})

	t.Run("a negative threshold applies the ratio ceiling with no lower bound", func(t *testing.T) {
		t.Parallel()
		// The same call that passed above, with the threshold negated — this
		// is the branch that makes a negative value meaningful rather than a
		// synonym for the default.
		err := check(5000, 100, 0, Options{MaxSize: 1 << 40, MaxRatio: 4, MinBombThreshold: -1})
		if !errors.Is(err, errTestBomb) {
			t.Fatalf("err = %v, want the ratio ceiling to apply with no lower bound", err)
		}
	})

	t.Run("an unknown archive size skips the ratio ceiling", func(t *testing.T) {
		t.Parallel()
		// arcSize 0 means "not known" (a streamed archive), and a ratio
		// against an unknown denominator is not a measurement.
		if err := check(1<<30, 0, 0, Options{MaxSize: 1 << 40, MaxRatio: 1, MinBombThreshold: 1}); err != nil {
			t.Errorf("err = %v, want nil when the archive size is unknown", err)
		}
	})
}

// TestNewBoundReader pins that the reader is configured from the same resolved
// defaults projectedBombCheck uses.
//
// That agreement is the helper's entire reason for existing: the projection and
// the enforcement-while-reading are two ceilings on one budget, and deriving
// them separately is how they drift apart. A reader built with raw opts values
// would enforce 0 rather than the default and let everything through.
func TestNewBoundReader(t *testing.T) {
	t.Parallel()

	var total int64
	src := strings.NewReader("payload")
	br := newBoundReader(src, &total, 4096, Options{}, "entry.bin", errTestBomb, "gotar")

	wantSize, wantRatio, wantThresh := bombLimitDefaults(Options{})
	if br.maxSize != wantSize || br.maxRatio != wantRatio || br.minThreshold != wantThresh {
		t.Errorf("limits = (%d, %d, %d), want the resolved defaults (%d, %d, %d)",
			br.maxSize, br.maxRatio, br.minThreshold, wantSize, wantRatio, wantThresh)
	}
	if br.totalRead != &total {
		t.Error("the reader does not share the caller's running total, so its accounting is private")
	}
	if br.arcSize != 4096 || br.name != "entry.bin" || br.errPrefix != "gotar" {
		t.Errorf("arcSize/name/errPrefix = (%d, %q, %q), want (4096, \"entry.bin\", \"gotar\")",
			br.arcSize, br.name, br.errPrefix)
	}
	if !errors.Is(br.errBomb, errTestBomb) {
		t.Errorf("errBomb = %v, want the caller's sentinel so each format keeps its own wording", br.errBomb)
	}
}
