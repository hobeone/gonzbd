package app

import (
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestJoinComponents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"single", []string{"config"}, "config"},
		{"two", []string{"app", "downloader"}, "app/downloader"},
		{"stage", []string{"app", "postproc", "repair"}, "app/postproc/repair"},
		{"engine", []string{"app", "postproc", "repair", "go_par2"}, "app/postproc/repair/go_par2"},
		{"unpack helper", []string{"app", "postproc", "unpack", "sevenzip"}, "app/postproc/unpack/sevenzip"},
		// Overlap merge: a compound value stacked under its own parent must not
		// double the shared segment.
		{"overlap compound", []string{"app", "postproc", "postproc/repair"}, "app/postproc/repair"},
		{"overlap nested", []string{"parent", "parent/child"}, "parent/child"},
		{"overlap two segments", []string{"app", "postproc/unpack", "unpack/sevenzip"}, "app/postproc/unpack/sevenzip"},
		{"empty values skipped", []string{"app", "", "repair"}, "app/repair"},
		// Full-length overlaps: the entire next value is already the tail, so it
		// contributes nothing (exercises i == len(parts), the loop's upper bound).
		{"duplicate token collapses", []string{"app", "app"}, "app"},
		{"trailing compound fully merges", []string{"app", "postproc", "repair", "postproc/repair"}, "app/postproc/repair"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := joinComponents(tt.in); got != tt.want {
				t.Errorf("joinComponents(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRecordWithSingleComponent(t *testing.T) {
	t.Parallel()

	collect := func(r slog.Record) []string {
		var comps []string
		var others []string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "component" {
				comps = append(comps, a.Value.String())
			} else {
				others = append(others, a.Key)
			}
			return true
		})
		return append(comps, others...)
	}

	t.Run("strips existing components and adds one joined", func(t *testing.T) {
		t.Parallel()
		r := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
		r.AddAttrs(slog.String("component", "app"), slog.String("job", "123"), slog.String("component", "postproc"))
		nr := recordWithSingleComponent(r, "app/postproc/repair")

		var comps []string
		var jobSeen bool
		nr.Attrs(func(a slog.Attr) bool {
			if a.Key == "component" {
				comps = append(comps, a.Value.String())
			}
			if a.Key == "job" {
				jobSeen = true
			}
			return true
		})
		if len(comps) != 1 || comps[0] != "app/postproc/repair" {
			t.Errorf("components = %v, want exactly [app/postproc/repair]", comps)
		}
		if !jobSeen {
			t.Error("non-component attr 'job' should be preserved")
		}
	})

	t.Run("empty component adds nothing", func(t *testing.T) {
		t.Parallel()
		r := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
		r.AddAttrs(slog.String("job", "123"))
		nr := recordWithSingleComponent(r, "")
		got := collect(nr)
		if !reflect.DeepEqual(got, []string{"job"}) {
			t.Errorf("attrs = %v, want [job] (no component added)", got)
		}
	})
}

func TestExtractComponents_Direct(t *testing.T) {
	t.Parallel()
	h := &filterHandler{}

	t.Run("empty record, no attrs", func(t *testing.T) {
		r := slog.Record{}
		got := h.extractComponents(r)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	t.Run("handler attributes only", func(t *testing.T) {
		h2 := &filterHandler{
			currentAttrs: []slog.Attr{
				slog.String("component", "api"),
				slog.String("other", "val"),
				slog.String("component", "api/auth"),
			},
		}
		r := slog.Record{}
		got := h2.extractComponents(r)
		want := []string{"api", "api/auth"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("record attributes only", func(t *testing.T) {
		h2 := &filterHandler{}
		r := slog.Record{}
		r.AddAttrs(
			slog.String("other", "val"),
			slog.String("component", "downloader"),
			slog.String("component", "downloader/connection"),
		)
		got := h2.extractComponents(r)
		want := []string{"downloader", "downloader/connection"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("combined handler and record attributes", func(t *testing.T) {
		h2 := &filterHandler{
			currentAttrs: []slog.Attr{
				slog.String("component", "app"),
			},
		}
		r := slog.Record{}
		r.AddAttrs(
			slog.String("component", "assembler"),
		)
		got := h2.extractComponents(r)
		want := []string{"app", "assembler"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}
