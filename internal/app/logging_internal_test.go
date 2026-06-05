package app

import (
	"log/slog"
	"reflect"
	"testing"
)

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
