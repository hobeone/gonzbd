package urlgrabber

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/hobeone/gonzbd/internal/types"
)

// nameRecorder records the filename each NZB reaches the handler under.
type nameRecorder struct{ names []string }

func (r *nameRecorder) HandleNZB(_ context.Context, filename string, _ []byte, _ types.FetchOptions) (string, error) {
	r.names = append(r.names, filename)
	return "id", nil
}

// TestFetchZipMembersNamedAfterMembers pins that each NZB in a downloaded zip
// reaches the handler under its own member name, and that members sharing a
// name, or whose name is rejected, still get distinct names.
func TestFetchZipMembersNamedAfterMembers(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{
		"Show.S01E01.nzb", "readme.txt", "sub/Show.S01E02.nzb",
		"other/Show.S01E02.nzb", "bad\x00name.nzb", "cp437-\xe9.nzb",
	} {
		fw, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte("<nzb/>")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer server.Close()

	h := &nameRecorder{}
	g := New(Config{AllowPrivateIPs: true}, h)
	if _, err := g.Fetch(t.Context(), server.URL+"/bundle.zip", types.FetchOptions{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	want := []string{
		"Show.S01E01.nzb", "Show.S01E02.nzb",
		"bundle.zip[3]", "bundle.zip[4]", "bundle.zip[5]",
	}
	if !slices.Equal(h.names, want) {
		t.Errorf("handler names = %q, want %q", h.names, want)
	}
}
