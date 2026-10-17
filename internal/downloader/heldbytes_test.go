package downloader

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/heldbytes"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// heldTestDownloader starts a downloader against a mock server holding n
// small articles, with a budget of budgetArticles articles' charge and a
// completions buffer that never fills, so the budget is the only bound.
func heldTestDownloader(t *testing.T, n, budgetArticles int) (*Downloader, int64) {
	t.Helper()
	ms := newMockNNTP(t)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("a%d@h", i)
		ms.addArticle(ids[i], string(yencBody("a.bin", []byte(fmt.Sprintf("body-%d", i)))))
	}
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, disp, j, m)

	// Every body here is far below the smallest buffer class, so each article
	// is charged exactly that class.
	charge := decoder.BufferCap(1)
	srv := testServer(t, "primary", ms.addr, func(c *config.ServerConfig) {
		c.Connections = 4
		c.PipeliningRequests = 2
	})
	d := New(disp, []*Server{srv}, nil, Options{
		CompletionsBuffer: 4 * n,
		HeldBytesLimit:    int64(budgetArticles) * charge,
	}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })
	return d, charge
}

// TestDownloader_SlowWriterCapsHeldBytesAtTheBudget is the cap itself: with
// nothing consuming results, decoded articles stop at the budget, however
// much room the completions channel has, and each release lets one more
// through. Every result released brings the budget back to zero.
func TestDownloader_SlowWriterCapsHeldBytesAtTheBudget(t *testing.T) {
	const articles, budget = 12, 3
	d, charge := heldTestDownloader(t, articles, budget)

	waitFor(t, "the budget to fill", func() bool { return len(d.completions) >= budget })
	// A negative observation: nothing consumes results, so a downloader the
	// budget did not stop would have fetched and emitted the other articles,
	// tiny bodies from a local server, well inside this window.
	time.Sleep(200 * time.Millisecond)
	if got := len(d.completions); got != budget {
		t.Fatalf("%d decoded results queued with nothing writing them, want the budget's %d", got, budget)
	}
	if got := d.held.Held(); got != budget*charge {
		t.Fatalf("Held() = %d, want %d", got, budget*charge)
	}

	for got := range articles {
		var r *ArticleResult
		select {
		case r = <-d.Completions():
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout after %d of %d results", got, articles)
		}
		if r.Err != nil {
			t.Fatalf("result %s: %v", r.MessageID, r.Err)
		}
		if held := d.held.Held(); held > budget*charge {
			t.Fatalf("Held() = %d exceeds the budget %d", held, budget*charge)
		}
		ackDone(t, d.dispatcher, r.JobID(), r.MessageID)
		r.Release()
	}
	if got := d.held.Held(); got != 0 {
		t.Fatalf("Held() = %d after every result was released, want 0", got)
	}
}

// TestDownloader_StopUnblocksFetchesWaitingOnTheBudget: a fetch waiting for
// room returns at shutdown holding nothing, so the only bytes still charged
// are those of the results already emitted, and releasing them empties the
// budget.
func TestDownloader_StopUnblocksFetchesWaitingOnTheBudget(t *testing.T) {
	const articles, budget = 12, 2
	d, charge := heldTestDownloader(t, articles, budget)

	waitFor(t, "the budget to fill", func() bool { return len(d.completions) == budget })

	stopped := make(chan error, 1)
	go func() { stopped <- d.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return: a fetch waiting on the budget ignored shutdown")
	}

	if got := d.held.Held(); got != int64(len(d.completions))*charge {
		t.Fatalf("Held() = %d after Stop with %d results queued, want %d",
			got, len(d.completions), int64(len(d.completions))*charge)
	}
	for len(d.completions) > 0 {
		(<-d.completions).Release()
	}
	if got := d.held.Held(); got != 0 {
		t.Fatalf("Held() = %d after the queued results were released, want 0", got)
	}
}

// TestDownloader_AFetchThatYieldsNoBodyGivesBackItsCharge: the charge is taken
// before the fetch, so a fetch that ends without a body (here a 430 for every
// article) must give it back, or a budget of one article would stop the
// downloader after the first miss.
func TestDownloader_AFetchThatYieldsNoBodyGivesBackItsCharge(t *testing.T) {
	ms := newMockNNTP(t)
	ids := []string{"m0@h", "m1@h", "m2@h"}
	for _, id := range ids {
		ms.rejectArticle(id)
	}
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, disp, j, m)
	d := New(disp, []*Server{testServer(t, "primary", ms.addr)}, nil, Options{
		HeldBytesLimit: decoder.BufferCap(1),
	}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	exhausted := map[string]bool{}
	deadline := time.After(10 * time.Second)
	for len(exhausted) < len(ids) {
		select {
		case r := <-d.Completions():
			if r.Err == nil {
				t.Fatalf("result %s succeeded, want a failure", r.MessageID)
			}
			if r.Held != nil {
				t.Fatalf("failure for %s carries a hold", r.MessageID)
			}
			if errors.Is(r.Err, ErrNoServersLeft) {
				exhausted[r.MessageID] = true
			}
		case <-deadline:
			t.Fatalf("only %d of %d articles reached ErrNoServersLeft; Held() = %d of a %d budget",
				len(exhausted), len(ids), d.held.Held(), decoder.BufferCap(1))
		}
	}
	waitFor(t, "the budget to empty", func() bool { return d.held.Held() == 0 })
}

func TestProcessFetchedArticle_ReleasesTheHoldOfAFailedDecode(t *testing.T) {
	budget := heldbytes.New(1 << 20)
	for name, body := range map[string][]byte{
		"not yEnc or UU": []byte("invalid data"),
		"CRC mismatch":   []byte("=ybegin line=128 size=12 part=1 name=t.txt\r\n=ypart begin=1 end=12\r\ntest content\r\n=yend size=12 part=1 pcrc32=12345678\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			d := New(newTestDispatcher(t), nil, nil, Options{}, nil)
			held, err := budget.Acquire(t.Context(), 1000)
			if err != nil {
				t.Fatal(err)
			}
			req := &articleRequest{job: bareJob("job1"), messageID: "msg1"}
			d.processFetchedArticle(t.Context(), checkedServer(config.ServerConfig{Name: "s1"}), req, body, held)
			if res := <-d.completions; res.Err == nil || res.Held != nil {
				t.Fatalf("result = (err %v, hold %v), want a failure carrying no hold", res.Err, res.Held)
			}
			if got := budget.Held(); got != 0 {
				t.Fatalf("Held() = %d after a failed decode, want 0", got)
			}
		})
	}
}

func TestProcessFetchedArticle_HandsTheHoldOnWithTheResult(t *testing.T) {
	budget := heldbytes.New(1 << 20)
	d := New(newTestDispatcher(t), nil, nil, Options{}, nil)
	held, err := budget.Acquire(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	req := &articleRequest{job: bareJob("job1"), messageID: "msg1", partNumber: 1}
	d.processFetchedArticle(t.Context(), checkedServer(config.ServerConfig{Name: "s1"}), req,
		yencBody("a.bin", []byte("payload")), held)
	res := <-d.completions
	if res.Err != nil || res.Held != held {
		t.Fatalf("result = (err %v, hold %p), want success carrying hold %p", res.Err, res.Held, held)
	}
	if got := budget.Held(); got != 1000 {
		t.Fatalf("Held() = %d while the result is unreleased, want 1000", got)
	}
	res.Release()
	if got := budget.Held(); got != 0 || res.Data != nil {
		t.Fatalf("after Release: Held() = %d, Data nil = %v; want 0 and true", got, res.Data == nil)
	}
}

// TestArticleResult_ReleaseHeldKeepsTheData: the assembler gives the bytes
// back through ReleaseHeld after it has finished with Data itself, so
// ReleaseHeld must release the hold and leave Data alone.
func TestArticleResult_ReleaseHeldKeepsTheData(t *testing.T) {
	budget := heldbytes.New(1 << 20)
	held, err := budget.Acquire(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	res := &ArticleResult{Data: decoder.GetBuffer(10), Held: held}
	res.ReleaseHeld()
	if got := budget.Held(); got != 0 || res.Data == nil {
		t.Fatalf("after ReleaseHeld: Held() = %d, Data nil = %v; want 0 and false", got, res.Data == nil)
	}
}

func TestEmitResult_ADroppedResultReleasesItsHold(t *testing.T) {
	budget := heldbytes.New(1 << 20)
	d := New(newTestDispatcher(t), nil, nil, Options{CompletionsBuffer: 1}, nil)
	d.completions <- &ArticleResult{} // full, so only the cancellation is ready

	held, err := budget.Acquire(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	j, m := makeJobWithArticles(t, []string{"a@h"})
	if err := j.AttachContent(m); err != nil {
		t.Fatal(err)
	}
	d.emitResult(ctx, &articleRequest{job: j, messageID: "a@h"}, "s1", decoder.GetBuffer(10), 0, 0, nil, held)
	if got := budget.Held(); got != 0 {
		t.Fatalf("Held() = %d after the result was dropped, want 0", got)
	}
}
