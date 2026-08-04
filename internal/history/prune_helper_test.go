package history

import (
	"testing"
)

// pruneVia runs the composition production uses: select the entries past
// retention, then delete them through Repository.Delete, which is also what
// releases their retained per-file progress. It returns the number removed.
//
// Repository.Prune used to do both halves itself with its own DELETE, which
// is precisely why it orphaned history_job_files rows (#303). Driving the
// tests through the same two calls the app makes keeps them honest about
// what actually happens when retention fires.
func pruneVia(t *testing.T, repo *Repository, retainDays, retainFailedDays int) int {
	t.Helper()

	expired, err := repo.ExpiredEntries(t.Context(), retainDays, retainFailedDays)
	if err != nil {
		t.Fatalf("ExpiredEntries: %v", err)
	}
	if len(expired) == 0 {
		return 0
	}
	ids := make([]string, len(expired))
	for i, e := range expired {
		ids[i] = e.NzoID
	}
	n, err := repo.Delete(t.Context(), ids...)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	return n
}
