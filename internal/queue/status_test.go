package queue

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
)

func TestCanTransitionStatus(t *testing.T) {
	legal := map[constants.Status][]constants.Status{
		constants.StatusQueued:      {constants.StatusDownloading, constants.StatusPaused, constants.StatusPropagating, constants.StatusFetching, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusDownloading: {constants.StatusQueued, constants.StatusPaused, constants.StatusFetching, constants.StatusVerifying, constants.StatusQuickCheck, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusPaused:      {constants.StatusQueued, constants.StatusDownloading, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusPropagating: {constants.StatusQueued, constants.StatusDownloading, constants.StatusPaused, constants.StatusDeleted},
		constants.StatusFetching:    {constants.StatusDownloading, constants.StatusQueued, constants.StatusVerifying, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusVerifying:   {constants.StatusQuickCheck, constants.StatusRepairing, constants.StatusExtracting, constants.StatusMoving, constants.StatusRunning, constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusQuickCheck:  {constants.StatusVerifying, constants.StatusRepairing, constants.StatusExtracting, constants.StatusMoving, constants.StatusRunning, constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusRepairing:   {constants.StatusVerifying, constants.StatusExtracting, constants.StatusMoving, constants.StatusRunning, constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusExtracting:  {constants.StatusRepairing, constants.StatusMoving, constants.StatusRunning, constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusMoving:      {constants.StatusRunning, constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusRunning:     {constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted},
		constants.StatusCompleted:   {constants.StatusDeleted},
		constants.StatusFailed:      {constants.StatusQueued, constants.StatusDeleted},
		constants.StatusDeleted:     {},
		constants.StatusIdle:        {constants.StatusQueued},
	}
	all := []constants.Status{
		constants.StatusQueued, constants.StatusDownloading, constants.StatusPaused,
		constants.StatusFetching, constants.StatusPropagating, constants.StatusChecking,
		constants.StatusQuickCheck, constants.StatusVerifying, constants.StatusRepairing,
		constants.StatusExtracting, constants.StatusMoving, constants.StatusRunning,
		constants.StatusCompleted, constants.StatusFailed, constants.StatusDeleted,
		constants.StatusIdle,
	}
	for _, from := range all {
		for _, to := range all {
			want := from == to // self-transition always legal
			for _, a := range legal[from] {
				if a == to {
					want = true
				}
			}
			if got := canTransitionStatus(from, to); got != want {
				t.Errorf("canTransitionStatus(%s -> %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestCanTransitionStatusSpotIllegal(t *testing.T) {
	illegal := []struct{ from, to constants.Status }{
		{constants.StatusCompleted, constants.StatusDownloading},
		{constants.StatusDeleted, constants.StatusQueued},
		{constants.StatusQueued, constants.StatusCompleted},
		{constants.StatusFailed, constants.StatusDownloading},
		{constants.StatusRunning, constants.StatusQueued},
	}
	for _, c := range illegal {
		if canTransitionStatus(c.from, c.to) {
			t.Errorf("canTransitionStatus(%s -> %s) = true, want false", c.from, c.to)
		}
	}
}

func TestErrIllegalStatusTransitionIsSentinel(t *testing.T) {
	err := illegalTransition(constants.StatusCompleted, constants.StatusQueued)
	if !errors.Is(err, ErrIllegalStatusTransition) {
		t.Error("illegalTransition must wrap ErrIllegalStatusTransition")
	}
}
