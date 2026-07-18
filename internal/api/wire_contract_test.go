package api

import (
	"reflect"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
)

// jsonTagsOf returns the json tag (name portion only, e.g. "foo" from
// "foo,omitempty") for every exported field of the given struct type,
// keyed by Go field name.
func jsonTagsOf(t *testing.T, v any) map[string]string {
	t.Helper()

	typ := reflect.TypeOf(v)
	got := make(map[string]string, typ.NumField())
	for f := range typ.Fields() {
		tag, ok := f.Tag.Lookup("json")
		if !ok {
			continue
		}
		// Strip options (",omitempty" etc.) — only the wire field name matters.
		for i, c := range tag {
			if c == ',' {
				tag = tag[:i]
				break
			}
		}
		got[f.Name] = tag
	}
	return got
}

// assertNoRemovedOrRenamedFields fails if any baseline (field name -> json
// tag) pair is missing or has changed in current. Additive fields in
// current are fine and intentionally not flagged — see issue #96: this
// guards against silent breaking changes (removed/renamed wire fields),
// not against growing the response.
func assertNoRemovedOrRenamedFields(t *testing.T, typeName string, baseline, current map[string]string) {
	t.Helper()

	for field, wantTag := range baseline {
		gotTag, ok := current[field]
		if !ok {
			t.Errorf("%s: field %q (json:%q) was removed — this is a public API wire type (issue #96); "+
				"if this is intentional, update the baseline in this test and confirm no client depends on it",
				typeName, field, wantTag)
			continue
		}
		if gotTag != wantTag {
			t.Errorf("%s: field %q json tag changed from %q to %q — this is a public API wire type (issue #96); "+
				"renaming breaks existing API clients (Sonarr/Radarr/NZB360/Glitter UI)",
				typeName, field, wantTag, gotTag)
		}
	}
}

// TestServerSnapshotWireContract guards downloader.ServerSnapshot's public
// JSON shape (served directly by internal/api and over the WebSocket in
// Event.Servers) against accidental field removal or renaming. See #96.
func TestServerSnapshotWireContract(t *testing.T) {
	baseline := map[string]string{
		"Name":           "name",
		"Host":           "host",
		"Port":           "port",
		"SSL":            "ssl",
		"Priority":       "priority",
		"Pipelining":     "pipelining",
		"MaxConnections": "max_connections",
		"ActiveConns":    "active_conns",
		"Active":         "active",
		"Enabled":        "enabled",
		"Optional":       "optional",
		"Required":       "required",
		"PenaltyUntil":   "penalty_until",
		"BPS":            "bps",
		"TotalBytes":     "total_bytes",
		"Connections":    "connections",
	}
	current := jsonTagsOf(t, downloader.ServerSnapshot{})
	assertNoRemovedOrRenamedFields(t, "downloader.ServerSnapshot", baseline, current)
}

// TestDirectUnpackStatusWireContract guards directunpack.Status's public
// JSON shape (served directly by internal/api via ServerStatus/queueSlot)
// against accidental field removal or renaming. See #96.
func TestDirectUnpackStatusWireContract(t *testing.T) {
	baseline := map[string]string{
		"Active":           "active",
		"CurrentSet":       "current_set",
		"CompletedVolumes": "completed_volumes",
		"TotalVolumes":     "total_volumes",
		"SuccessSets":      "success_sets",
		"FailedSets":       "failed_sets",
		"FailedReasons":    "failed_reasons",
	}
	current := jsonTagsOf(t, directunpack.Status{})
	assertNoRemovedOrRenamedFields(t, "directunpack.Status", baseline, current)
}

// TestConnSnapshotWireContract guards downloader.ConnSnapshot's public JSON
// shape (nested in ServerSnapshot.Connections, same public response) against
// accidental field removal or renaming. See #96.
func TestConnSnapshotWireContract(t *testing.T) {
	baseline := map[string]string{
		"Index":     "index",
		"ArticleID": "article_id",
		"Subject":   "subject",
		"Bytes":     "bytes",
		"SinceUnix": "since_unix",
		"Connected": "connected",
	}
	current := jsonTagsOf(t, downloader.ConnSnapshot{})
	assertNoRemovedOrRenamedFields(t, "downloader.ConnSnapshot", baseline, current)
}
