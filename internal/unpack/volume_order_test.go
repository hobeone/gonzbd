package unpack

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestOrderVolumes pins the ordering rarengine depends on.
//
// Archive.Parts is sorted for DELETION, not for playback: legacy ".r00" sorts
// lexicographically before the main ".rar", so handing that order straight to
// rarengine feeds it the volumes back to front and it aborts. New-style
// ".partNN.rar" sets already sort correctly and must be left alone, because
// reordering them on a wrong guess is the same defect in the other direction.
func TestOrderVolumes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mainFile   string
		candidates []string
		want       []string
	}{
		{
			name:       "a single volume is returned unchanged",
			mainFile:   "show.rar",
			candidates: []string{"show.rar"},
			want:       []string{"show.rar"},
		},
		{
			name:     "legacy volumes are sequenced numerically after the main file",
			mainFile: "show.rar",
			// Lexicographic order, which is what deletion sorting produces.
			candidates: []string{"show.r00", "show.r01", "show.r02", "show.rar"},
			want:       []string{"show.rar", "show.r00", "show.r01", "show.r02"},
		},
		{
			name:     "a numeric suffix past 9 sorts by value, not by text",
			mainFile: "show.rar",
			// ".r10" precedes ".r9" lexicographically; only Atoi gets this right.
			candidates: []string{"show.r9", "show.r10", "show.rar"},
			want:       []string{"show.rar", "show.r9", "show.r10"},
		},
		{
			name:     "new-style part volumes keep the caller's order",
			mainFile: "show.part01.rar",
			candidates: []string{
				"show.part01.rar", "show.part02.rar", "show.part03.rar",
			},
			want: []string{
				"show.part01.rar", "show.part02.rar", "show.part03.rar",
			},
		},
		{
			name:     "a set that is not all legacy is left alone",
			mainFile: "show.rar",
			// One unrecognized name means the legacy assumption does not hold
			// for the set, so reordering would be a guess.
			candidates: []string{"show.r00", "show.zzz", "show.rar"},
			want:       []string{"show.r00", "show.zzz", "show.rar"},
		},
		{
			name:       "an empty candidate list is returned unchanged",
			mainFile:   "show.rar",
			candidates: nil,
			want:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := orderVolumes(tt.mainFile, tt.candidates)
			if !slices.Equal(got, tt.want) {
				t.Errorf("orderVolumes(%q, %v) = %v, want %v", tt.mainFile, tt.candidates, got, tt.want)
			}
		})
	}
}

// TestDiscoverLegacyVolumes pins the stop-at-first-gap rule.
//
// The probe walks .r00, .r01, … and stops at the first index that is missing.
// That is deliberate rather than lazy: a gap means the set is incomplete, and
// continuing past it would hand rarengine a discontinuous volume list, which
// fails later and less legibly than a short one.
func TestDiscoverLegacyVolumes(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, dir, name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { //nolint:gosec // fixture under t.TempDir()
			t.Fatal(err)
		}
		return p
	}

	t.Run("a contiguous run is collected in order", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		main := write(t, dir, "show.rar")
		r00 := write(t, dir, "show.r00")
		r01 := write(t, dir, "show.r01")

		got, err := discoverLegacyVolumes(main)
		if err != nil {
			t.Fatalf("discoverLegacyVolumes: %v", err)
		}
		if want := []string{main, r00, r01}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("collection stops at the first missing index", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		main := write(t, dir, "show.rar")
		r00 := write(t, dir, "show.r00")
		// .r01 is absent; .r02 exists but is unreachable past the gap.
		write(t, dir, "show.r02")

		got, err := discoverLegacyVolumes(main)
		if err != nil {
			t.Fatalf("discoverLegacyVolumes: %v", err)
		}
		if want := []string{main, r00}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v — collection must stop at the gap rather than skip it", got, want)
		}
	})

	t.Run("a main file with no siblings yields just itself", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		main := write(t, dir, "solo.rar")

		got, err := discoverLegacyVolumes(main)
		if err != nil {
			t.Fatalf("discoverLegacyVolumes: %v", err)
		}
		if want := []string{main}; !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// TestDiscoverRar5Volumes pins which naming convention each main file selects.
//
// The choice is made on the NAME, before any probing, so a misrouted main file
// discovers nothing and the extraction proceeds with a single volume.
// Volume discovery operates on the base name of the mainFile so that parent
// directory components containing ".part" do not misroute discovery or corrupt
// sibling volume paths.
func TestDiscoverRar5Volumes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		subdir   string
		files    []string
		mainFile string
		want     []string
	}{
		{
			name:     "a multipart name routes to the numbered-volume prober",
			files:    []string{"show.part01.rar", "show.part02.rar"},
			mainFile: "show.part01.rar",
			want:     []string{"show.part01.rar", "show.part02.rar"},
		},
		{
			name:     "when parent directory contains .part it correctly routes to numbered-volume prober",
			subdir:   "release.part.1",
			files:    []string{"show.part01.rar", "show.part02.rar"},
			mainFile: "show.part01.rar",
			want:     []string{"show.part01.rar", "show.part02.rar"},
		},
		{
			name:     "a name with .rar but .part in directory path routes to legacy prober",
			subdir:   "some.part",
			files:    []string{"show.rar", "show.r00"},
			mainFile: "show.rar",
			want:     []string{"show.rar", "show.r00"},
		},
		{
			name:     "multiple .part tokens in base name matches trailing volume part",
			files:    []string{"Movie.part1.of.2.part01.rar", "Movie.part1.of.2.part02.rar"},
			mainFile: "Movie.part1.of.2.part01.rar",
			want:     []string{"Movie.part1.of.2.part01.rar", "Movie.part1.of.2.part02.rar"},
		},
		{
			name:     "a plain .rar name routes to the legacy prober",
			files:    []string{"show.rar", "show.r00"},
			mainFile: "show.rar",
			want:     []string{"show.rar", "show.r00"},
		},
		{
			name:     "any other extension is treated as a single volume",
			mainFile: "show.7z",
			want:     []string{"show.7z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.subdir != "" {
				dir = filepath.Join(dir, tt.subdir)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, n := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil { //nolint:gosec // fixture
					t.Fatal(err)
				}
			}

			main := filepath.Join(dir, tt.mainFile)
			got, err := discoverRar5Volumes(main)
			if err != nil {
				t.Fatalf("discoverRar5Volumes(%q): %v", main, err)
			}

			want := make([]string, len(tt.want))
			for i, w := range tt.want {
				want[i] = filepath.Join(dir, w)
			}
			if !slices.Equal(got, want) {
				t.Errorf("discoverRar5Volumes(%q) = %v, want %v", main, got, want)
			}
		})
	}
}

// TestVolumesForArchive pins which source of volumes wins.
//
// A caller-supplied Parts list is authoritative and suppresses filesystem
// probing entirely; probing is the fallback for hand-built Archive values that
// bypass Scan. Probing when Parts was supplied would let files on disk override
// what the caller asked for.
func TestVolumesForArchive(t *testing.T) {
	t.Parallel()

	t.Run("a multi-part list is ordered without probing", func(t *testing.T) {
		t.Parallel()
		// None of these exist on disk. If volumesForArchive probed, it could
		// not return them.
		archive := Archive{
			MainFile: "/nonexistent/show.rar",
			Parts: []string{
				"/nonexistent/show.r00", "/nonexistent/show.r01", "/nonexistent/show.rar",
			},
		}

		got, err := volumesForArchive(archive)
		if err != nil {
			t.Fatalf("volumesForArchive: %v", err)
		}
		want := []string{"/nonexistent/show.rar", "/nonexistent/show.r00", "/nonexistent/show.r01"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("a single part falls back to probing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for _, n := range []string{"show.rar", "show.r00"} {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil { //nolint:gosec // fixture
				t.Fatal(err)
			}
		}
		main := filepath.Join(dir, "show.rar")

		got, err := volumesForArchive(Archive{MainFile: main, Parts: []string{main}})
		if err != nil {
			t.Fatalf("volumesForArchive: %v", err)
		}
		want := []string{main, filepath.Join(dir, "show.r00")}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v — a lone Parts entry means Scan supplied nothing to trust", got, want)
		}
	})
}
