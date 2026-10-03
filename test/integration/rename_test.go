//go:build integration

package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestAPI_RenameCannotEscapeTheJobDirectory drives queue&name=rename through
// the real application. A job's name is its download directory, so no rename
// may yield ".", "..", a path separator, nothing, or another job's name.
func TestAPI_RenameCannotEscapeTheJobDirectory(t *testing.T) {
	t.Parallel()
	srv, ts := buildAPIServerWithQueue(t)
	id := addJobToQueue(t, srv, BuildNZB([]TestFile{{Name: "one.bin", Payload: []byte("one")}}), "one")
	addJobToQueue(t, srv, BuildNZB([]TestFile{{Name: "two.bin", Payload: []byte("two")}}), "two")

	rename := func(name string) int {
		resp := apiDo(t, ts, "mode=queue&name=rename&value="+id+"&value2="+url.QueryEscape(name)+"&apikey="+integrationAPIKey)
		defer resp.Body.Close() //nolint:errcheck // test
		return resp.StatusCode
	}
	nameNow := func() string {
		row, ok := srv.Dispatcher().Row(id)
		if !ok {
			t.Fatalf("job %s is gone", id)
		}
		return row.Header.Name
	}

	for _, bad := range []string{".", "..", " . "} {
		if code := rename(bad); code != http.StatusBadRequest {
			t.Errorf("rename to %q: status %d, want 400", bad, code)
		}
		if got := nameNow(); got != "one" {
			t.Errorf("after a refused rename to %q the name is %q", bad, got)
		}
	}
	if code := rename(""); code != http.StatusBadRequest {
		t.Errorf("rename to an empty name: status %d, want 400", code)
	}

	if code := rename("a/b"); code != http.StatusOK {
		t.Fatalf("rename to a/b: status %d, want 200", code)
	}
	if got := nameNow(); strings.ContainsAny(got, `/\`) {
		t.Errorf("rename to a/b stored %q, a path", got)
	}

	if code := rename("two"); code != http.StatusOK {
		t.Fatalf("rename to another job's name: status %d, want 200", code)
	}
	if got := nameNow(); got == "two" {
		t.Error("the rename took another job's name; the two would share a download directory")
	}
}
