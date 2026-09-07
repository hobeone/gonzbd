package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/dispatch"
	dispatchstore "github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

type failDeleteStore struct {
	dispatch.Store
	mu        sync.Mutex
	attempts  int
	failCount int
}

func (s *failDeleteStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.attempts <= s.failCount {
		return errors.New("simulated store delete failure")
	}
	return s.Store.Delete(ctx, id)
}

func (s *failDeleteStore) getAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func newAppWithCustomDispatchStore(t *testing.T, failCount int) (*Application, *job.Job, *failDeleteStore) {
	t.Helper()
	application, repo, _ := newLifecycleTestApp(t)
	s := &failDeleteStore{
		Store:     dispatchstore.New(repo.DB()),
		failCount: failCount,
	}
	application.dispatcher = dispatch.New(
		4, 2, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		s,
		application.runner,
	)
	application.runner.report = application.dispatcher

	j := job.New("test-remove-job", "test-job", job.Policy{})
	hdr := dispatch.Header{
		Name:  "test-job",
		Bytes: 100,
	}
	if err := application.dispatcher.Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return application, j, s
}

type captureEmitter struct {
	mu     sync.Mutex
	events []Event
}

func (c *captureEmitter) Broadcast(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureEmitter) hasEventType(eventType string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Type == eventType {
			return true
		}
	}
	return false
}

func TestFinalizer_RemoveError_RetriesAndSucceeds(t *testing.T) {
	t.Parallel()

	application, j, s := newAppWithCustomDispatchStore(t, 1)

	entry := history.Entry{
		NzoID:  j.ID(),
		Name:   j.Name(),
		Status: string(constants.StatusCompleted),
	}
	ppJob := &postproc.Job{Job: j}

	if err := application.finalizer.persistAndCommit(slog.Default(), entry, ppJob); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}

	if attempts := s.getAttempts(); attempts != 2 {
		t.Errorf("expected 2 delete attempts (1 failure + 1 retry), got %d", attempts)
	}

	if _, ok := application.dispatcher.Job(j.ID()); ok {
		t.Errorf("expected job %s to be removed from dispatcher after retry, but it was found", j.ID())
	}
}

func TestFinalizer_RemoveError_ExhaustedRetry_SurfacesWarning(t *testing.T) {
	t.Parallel()

	application, j, s := newAppWithCustomDispatchStore(t, 10)

	emitter := &captureEmitter{}
	application.emitter = emitter

	entry := history.Entry{
		NzoID:  j.ID(),
		Name:   j.Name(),
		Status: string(constants.StatusCompleted),
	}
	ppJob := &postproc.Job{Job: j}

	if err := application.finalizer.persistAndCommit(slog.Default(), entry, ppJob); err != nil {
		t.Fatalf("persistAndCommit: %v", err)
	}

	if attempts := s.getAttempts(); attempts != 2 {
		t.Errorf("expected 2 delete attempts, got %d", attempts)
	}

	row, ok := application.dispatcher.Row(j.ID())
	if !ok {
		t.Fatalf("expected job %s to remain registered in dispatcher, but Row returned false", j.ID())
	}

	if !strings.Contains(row.Header.Warning, "failed to remove finalized job from queue") {
		t.Errorf("expected warning to contain %q, got %q", "failed to remove finalized job from queue", row.Header.Warning)
	}

	if !emitter.hasEventType("queue_updated") {
		t.Errorf("expected emitter to broadcast queue_updated event, but it did not")
	}
}
