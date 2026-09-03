package downloader

import (
	"context"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/job"
)

type testWorkers struct{}

func (testWorkers) Abort(string) {}

type testResidency struct{}

func (testResidency) Hydrate(context.Context, string) error { return nil }
func (testResidency) Evict(string)                          {}

type testStore struct{}

func (testStore) Load(context.Context) ([]dispatch.Persisted, error) { return nil, nil }
func (testStore) Save(context.Context, dispatch.Persisted) error     { return nil }
func (testStore) Delete(context.Context, string) error               { return nil }

type testRunner struct{}

func (testRunner) Run(context.Context, string, job.State) {}

func newTestDispatcher(t *testing.T) *dispatch.Dispatcher {
	if t != nil {
		t.Helper()
	}
	return dispatch.New(
		10, 10, time.Hour,
		func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		testWorkers{},
		testResidency{},
		testStore{},
		testRunner{},
	)
}

func addTestJob(t *testing.T, disp *dispatch.Dispatcher, j *job.Job, m *job.Manifest) *job.Job {
	if t != nil {
		t.Helper()
	}
	if err := disp.Add(j, dispatch.Header{Name: j.ID()}); err != nil {
		if t != nil {
			t.Fatalf("disp.Add: %v", err)
		}
	}
	if m != nil {
		if err := j.AttachContent(m); err != nil {
			if t != nil {
				t.Fatalf("AttachContent: %v", err)
			}
		}
	}
	disp.Tick(context.Background())
	disp.Tick(context.Background())
	return j
}

func makeJobWithArticles(t *testing.T, msgIDs []string) (*job.Job, *job.Manifest) {
	if t != nil {
		t.Helper()
	}
	j := job.New("j1", "test.nzb", job.Policy{})
	arts := make([]job.JobArticle, len(msgIDs))
	for i, id := range msgIDs {
		arts[i] = job.JobArticle{
			ID:     id,
			Bytes:  100,
			Number: i + 1,
		}
	}
	file := job.JobFile{
		Subject:  "test.bin",
		Articles: arts,
		Bytes:    int64(100 * len(msgIDs)),
	}
	m := job.NewManifest([]job.JobFile{file})
	return j, m
}
