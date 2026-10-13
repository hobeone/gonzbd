package directunpack

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The helpers below were untested when this file was added. They are
// small, but each owns a piece of state the rest of the package reads, and
// check_test_alignment surfaced them as gaps in a file a fix had touched.

func TestBuildVolumeMap(t *testing.T) {
	tests := []struct {
		name      string
		filenames []string
		want      map[string]int
	}{
		{
			name:      "no filenames leaves the map empty",
			filenames: nil,
			want:      map[string]int{},
		},
		{
			name:      "highest volume number wins",
			filenames: []string{"movie.part01.rar", "movie.part03.rar", "movie.part02.rar"},
			want:      map[string]int{"movie": 3},
		},
		{
			name:      "a lower volume seen later does not lower the total",
			filenames: []string{"movie.part09.rar", "movie.part01.rar"},
			want:      map[string]int{"movie": 9},
		},
		{
			name:      "sets are tracked independently",
			filenames: []string{"movie.part02.rar", "show.part05.rar"},
			want:      map[string]int{"movie": 2, "show": 5},
		},
		{
			name:      "names that analyze to no set are skipped",
			filenames: []string{"readme.txt", "movie.part01.rar", "notes.nfo"},
			want:      map[string]int{"movie": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &DirectUnpacker{
				allFilenames: tt.filenames,
				totalVolumes: make(map[string]int),
			}

			d.buildVolumeMap()

			if len(d.totalVolumes) != len(tt.want) {
				t.Fatalf("totalVolumes = %v, want %v", d.totalVolumes, tt.want)
			}
			for set, vol := range tt.want {
				if got := d.totalVolumes[set]; got != vol {
					t.Errorf("totalVolumes[%q] = %d, want %d", set, got, vol)
				}
			}
		})
	}
}

func TestSetAdmitted(t *testing.T) {
	tests := []struct {
		name       string
		nextSets   []string
		curSetname string
		settled    func(d *DirectUnpacker)
		query      string
		want       bool
	}{
		{"queued behind others", []string{"a", "b"}, "cur", nil, "b", true},
		{"the first queued set", []string{"a", "b"}, "cur", nil, "a", true},
		{"the set currently extracting", nil, "cur", nil, "cur", true},
		{"neither queued nor current", []string{"a"}, "cur", nil, "other", false},
		{"nothing queued and nothing current", nil, "", nil, "a", false},
		{"queued while nothing is extracting", []string{"a"}, "", nil, "a", true},
		{"a set that succeeded", nil, "cur", func(d *DirectUnpacker) { d.successSets["a"] = SuccessSet{} }, "a", true},
		{"a set that failed", nil, "cur", func(d *DirectUnpacker) { d.failedSets["a"] = FailedSet{} }, "a", true},
		{"a set that was skipped", nil, "cur", func(d *DirectUnpacker) { d.skippedSets["a"] = SkippedSet{} }, "a", true},
	}

	// setAdmitted("") is deliberately not a case here. Add returns before
	// touching any state when AnalyzeRarFilename yields "" (directunpack.go,
	// "not a RAR volume"), so "" never reaches nextSets or curSetname and the
	// question is unreachable. It answers true on an idle unpacker only
	// because `setname == d.curSetname` degenerates to `"" == ""`, which is
	// the zero-value collision Standing Design Rule 2 names -- pinning it as
	// intended behaviour would make a later guard against it look like a
	// regression.

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(slog.New(slog.DiscardHandler), "job", "", "", Options{})
			d.nextSets = slices.Clone(tt.nextSets)
			d.curSetname = tt.curSetname
			if tt.settled != nil {
				tt.settled(d)
			}

			if got := d.setAdmitted(tt.query); got != tt.want {
				t.Errorf("setAdmitted(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestSettleLocked(t *testing.T) {
	tests := []struct {
		name        string
		killed      bool
		err         error
		wantSuccess bool
		wantSkip    string
		wantFail    string
	}{
		{"a published set", false, nil, true, "", ""},
		{"a published set after Abort", true, nil, true, "", ""},
		{"not RAR5", false, errNotRAR, false, "not RAR5; handled by normal unpack", ""},
		{"a queued set at Abort", true, errNotStarted, false, "", "aborted (not started)"},
		{"an error after Abort", true, errors.New("killed"), false, "", "aborted"},
		{"an error", false, errors.New("bad CRC"), false, "", "bad CRC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(slog.New(slog.DiscardHandler), "job", "", "", Options{})
			d.killed = tt.killed
			res := SuccessSet{ExtractedFiles: []string{"f"}}

			d.settleLocked("s", res, tt.err)

			if _, ok := d.successSets["s"]; ok != tt.wantSuccess {
				t.Errorf("successSets has s = %v, want %v", ok, tt.wantSuccess)
			}
			if got := d.skippedSets["s"].Reason; got != tt.wantSkip {
				t.Errorf("skip reason = %q, want %q", got, tt.wantSkip)
			}
			if got := d.failedSets["s"].Reason; got != tt.wantFail {
				t.Errorf("failure reason = %q, want %q", got, tt.wantFail)
			}
		})
	}
}

// TestExtractEntries_ExitsBeforeReadingTheArchive covers the three ways
// extractEntries returns without consuming an entry. Each is reached before the
// first r.NextEntry() call, which is why a nil Reader is a sufficient argument
// here and why these cases need no archive fixture. A change that moved any of
// these checks below the read would dereference the nil and fail loudly rather
// than silently stop being an early exit.
func TestExtractEntries_ExitsBeforeReadingTheArchive(t *testing.T) {
	t.Run("an unusable stage dir is reported", func(t *testing.T) {
		notADir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		d := &DirectUnpacker{extractDir: filepath.Dir(notADir)}

		err := d.extractEntries(context.Background(), nil, notADir)

		if err == nil {
			t.Fatal("extractEntries() error = nil, want an open-root failure")
		}
		if !strings.Contains(err.Error(), "open root") {
			t.Errorf("error = %v, want it to mention open root", err)
		}
	})

	t.Run("a cancelled context stops the loop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dir := t.TempDir()
		d := &DirectUnpacker{extractDir: dir}

		err := d.extractEntries(ctx, nil, dir)

		if !errors.Is(err, context.Canceled) {
			t.Errorf("extractEntries() error = %v, want context.Canceled", err)
		}
	})

	t.Run("an aborted unpacker stops the loop", func(t *testing.T) {
		dir := t.TempDir()
		d := &DirectUnpacker{extractDir: dir, killed: true}

		err := d.extractEntries(context.Background(), nil, dir)

		if err == nil || !strings.Contains(err.Error(), "killed") {
			t.Errorf("extractEntries() error = %v, want it to mention killed", err)
		}
	})
}

func TestAwaitNextSet(t *testing.T) {
	newD := func() *DirectUnpacker { return New(slog.New(slog.DiscardHandler), "job", "", "", Options{}) }

	t.Run("an aborted unpacker exits", func(t *testing.T) {
		d := newD()
		d.killed = true
		d.nextSets = []string{"a"}
		if s, ok := d.awaitNextSet(t.Context()); ok || s != "" || !d.finished {
			t.Errorf("awaitNextSet = %q, %v, finished %v; want an exit with finished set", s, ok, d.finished)
		}
	})
	t.Run("a queued set becomes current", func(t *testing.T) {
		d := newD()
		d.nextSets = []string{"a", "b"}
		d.inputEnded = true
		if s, ok := d.awaitNextSet(t.Context()); !ok || s != "a" || d.curSetname != "a" || !slices.Equal(d.nextSets, []string{"b"}) {
			t.Errorf("awaitNextSet = %q, %v; current %q, queue %q; want a taken from the queue", s, ok, d.curSetname, d.nextSets)
		}
	})
	t.Run("an empty queue after the input ended exits", func(t *testing.T) {
		d := newD()
		d.inputEnded = true
		if _, ok := d.awaitNextSet(t.Context()); ok || !d.finished {
			t.Errorf("awaitNextSet ok %v, finished %v; want an exit with finished set", ok, d.finished)
		}
	})
	t.Run("an empty queue after ctx is done exits", func(t *testing.T) {
		d := newD()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, ok := d.awaitNextSet(ctx); ok || !d.finished {
			t.Errorf("awaitNextSet ok %v, finished %v; want an exit with finished set", ok, d.finished)
		}
	})
	t.Run("an empty queue waits for a set", func(t *testing.T) {
		d := newD()
		got := make(chan string, 1)
		go func() {
			s, _ := d.awaitNextSet(t.Context())
			got <- s
		}()
		select {
		case s := <-got:
			t.Fatalf("awaitNextSet returned %q with nothing queued and the input open", s)
		case <-time.After(50 * time.Millisecond):
		}
		d.mu.Lock()
		d.nextSets = append(d.nextSets, "late")
		d.mu.Unlock()
		signal(d.queued)
		select {
		case s := <-got:
			if s != "late" {
				t.Errorf("awaitNextSet = %q, want late", s)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("awaitNextSet never took the set queued while it waited")
		}
	})
}
