package main

import (
	"reflect"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// storeMethodExclusions are queue.Store methods deliberately absent from
// storeMethods, with the reason. Keep in sync with storeMethods' own doc
// comment.
var storeMethodExclusions = map[string]string{
	"Get": "too generic to attribute — collides with dirscanner's in-memory store",
}

// TestStoreMethodsMatchStoreInterface pins storeMethods against the real
// queue.Store interface.
//
// storeMethods is a hand-maintained name table, and a name in it that no
// longer matches a real method does not fail anything: the detector simply
// never matches that call, so the gate silently stops covering it and still
// reports success. That is what happened to ArticleCountsByJob, which was
// registered as "ArticleCountsByFile" and therefore went unchecked from the
// day it was added. A green run of a gate that has quietly narrowed its own
// scope is worse than a red one.
//
// Both directions matter, so both are checked: an unregistered method is an
// uncovered I/O path, and a registered name with no matching method is a
// typo that has already silently disabled a check.
func TestStoreMethodsMatchStoreInterface(t *testing.T) {
	storeType := reflect.TypeFor[queue.Store]()

	actual := make(map[string]bool, storeType.NumMethod())
	for m := range storeType.Methods() {
		actual[m.Name] = true
	}

	for name := range actual {
		if storeMethods[name] {
			continue
		}
		if reason, ok := storeMethodExclusions[name]; ok {
			t.Logf("queue.Store.%s excluded from storeMethods: %s", name, reason)
			continue
		}
		t.Errorf("queue.Store.%s is not registered in storeMethods, so calls to it are never checked for I/O under a lock; add it, or add it to storeMethodExclusions with a reason", name)
	}

	// "Save" is intentionally present without being on queue.Store — the
	// doc comment records that it belongs to a different store that also
	// writes to disk. Everything else must resolve to a real method.
	knownNonStore := map[string]bool{"Save": true}

	for name := range storeMethods {
		if actual[name] || knownNonStore[name] {
			continue
		}
		t.Errorf("storeMethods registers %q, which is not a method on queue.Store; the detector will never match it, silently disabling that check", name)
	}

	for name := range storeMethodExclusions {
		if !actual[name] {
			t.Errorf("storeMethodExclusions names %q, which is no longer a method on queue.Store; drop the stale exclusion", name)
		}
	}
}
