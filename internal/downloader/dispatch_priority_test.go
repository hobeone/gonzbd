package downloader

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp"
)

// The tests in this file pin strict server-priority tiers (#985): an article
// is offered to a priority only once every server of a better priority has
// tried it or is not active, and a full work channel on an active better
// server makes the article wait rather than overflow.

// drained reports whether ch holds no request, without blocking.
func drained(ch chan *articleRequest) bool {
	select {
	case req := <-ch:
		ch <- req
		return false
	default:
		return true
	}
}

func TestTryDispatch_FullPrimaryHoldsTheArticleRatherThanOverflowing(t *testing.T) {
	t.Parallel()

	primary := fakeSrv("primary", 0, true)
	backup := fakeSrv("backup", 1, true)
	d := newDispatchDownloader([]*Server{primary, backup})
	d.workCh["primary"] <- &articleRequest{messageID: "blocker"}

	handled, exReq, _ := d.tryDispatch(context.Background(), fakeArticle("msg1@h"), defaultOpts(d.servers))

	if handled || exReq != nil {
		t.Fatalf("tryDispatch = (%v, %v), want (false, nil): the article waits for the full primary", handled, exReq)
	}
	if !drained(d.workCh["backup"]) {
		t.Error("backup received the article while the primary was active and had not tried it")
	}
}

func TestTryDispatch_AnArticleThePrimaryTriedGoesToTheBackup(t *testing.T) {
	t.Parallel()

	primary := fakeSrv("primary", 0, true)
	backup := fakeSrv("backup", 1, true)
	d := newDispatchDownloader([]*Server{primary, backup})
	a := fakeArticle("msg1@h")
	var mask serverMask
	mask.set(0)
	d.tracker.Lock()
	d.tracker.SetTriedLocked(keyFor(a.Job, a.ArtIdx), mask)
	d.tracker.Unlock()

	handled, exReq, _ := d.tryDispatch(context.Background(), a, defaultOpts(d.servers))

	if !handled || exReq != nil {
		t.Fatalf("tryDispatch = (%v, %v), want (true, nil)", handled, exReq)
	}
	if drained(d.workCh["backup"]) {
		t.Error("backup did not receive an article the primary has already tried")
	}
}

// A waiting article is re-evaluated on the next call: once the primary is
// penalized it no longer holds the article back.
func TestTryDispatch_AWaitingArticleMovesDownWhenThePrimaryIsPenalized(t *testing.T) {
	t.Parallel()

	primary := fakeSrv("primary", 0, true)
	backup := fakeSrv("backup", 1, true)
	d := newDispatchDownloader([]*Server{primary, backup})
	d.workCh["primary"] <- &articleRequest{messageID: "blocker"}
	a := fakeArticle("msg1@h")

	if handled, _, _ := d.tryDispatch(context.Background(), a, defaultOpts(d.servers)); handled {
		t.Fatal("article dispatched while the full primary was active")
	}

	primary.ApplyPenalty(time.Hour)
	handled, exReq, _ := d.tryDispatch(context.Background(), a, defaultOpts(d.servers))

	if !handled || exReq != nil {
		t.Fatalf("after the penalty tryDispatch = (%v, %v), want (true, nil)", handled, exReq)
	}
	if drained(d.workCh["backup"]) {
		t.Error("backup did not receive the article once the primary was penalized")
	}
}

// Priority, not the order servers are configured in, picks the tier.
func TestTryDispatch_PriorityOutranksConfigOrder(t *testing.T) {
	t.Parallel()

	backup := fakeSrv("backup", 1, true)
	primary := fakeSrv("primary", 0, true)
	d := newDispatchDownloader([]*Server{backup, primary})

	handled, _, _ := d.tryDispatch(context.Background(), fakeArticle("msg1@h"), defaultOpts(d.servers))

	if !handled {
		t.Fatal("article not dispatched")
	}
	if drained(d.workCh["primary"]) {
		t.Error("primary did not receive the article; the backup listed first took it")
	}
}

// With the best tier inactive, the next tier is the one an article waits for:
// a full middle tier does not let the article skip to the last.
func TestTryDispatch_AFullMiddleTierHoldsTheArticleWhenTheTopIsPenalized(t *testing.T) {
	t.Parallel()

	top := fakeSrv("top", 0, true)
	middle := fakeSrv("middle", 1, true)
	bottom := fakeSrv("bottom", 2, true)
	d := newDispatchDownloader([]*Server{top, middle, bottom})
	top.ApplyPenalty(time.Hour)
	d.workCh["middle"] <- &articleRequest{messageID: "blocker"}

	handled, exReq, _ := d.tryDispatch(context.Background(), fakeArticle("msg1@h"), defaultOpts(d.servers))

	if handled || exReq != nil {
		t.Fatalf("tryDispatch = (%v, %v), want (false, nil)", handled, exReq)
	}
	if !drained(d.workCh["bottom"]) {
		t.Error("bottom tier received the article while the middle tier was active")
	}
}

// Servers sharing a priority still balance: a full one passes the article to
// its peer. TestTryDispatch_WorkChFullSkipsToNextServer pins the same at
// priority 0; this one puts the shared tier below a penalized top.
func TestTryDispatch_ServersSharingATierBalanceBelowAPenalizedTop(t *testing.T) {
	t.Parallel()

	top := fakeSrv("top", 0, true)
	peerA := fakeSrv("peerA", 1, true)
	peerB := fakeSrv("peerB", 1, true)
	d := newDispatchDownloader([]*Server{top, peerA, peerB})
	top.ApplyPenalty(time.Hour)
	d.workCh["peerA"] <- &articleRequest{messageID: "blocker"}

	handled, _, _ := d.tryDispatch(context.Background(), fakeArticle("msg1@h"), defaultOpts(d.servers))

	if !handled {
		t.Fatal("article not dispatched to the free peer")
	}
	if drained(d.workCh["peerB"]) {
		t.Error("peerB did not receive the article while its peer was full")
	}
}

// An article that needs the backup is not stuck behind the articles waiting
// for the primary: the pass reaches it although every article before it waits.
func TestBuildDispatchPlan_AnArticleNeedingTheBackupIsNotStuckBehindWaitingOnes(t *testing.T) {
	t.Parallel()

	primary := fakeSrv("primary", 0, true)
	backup := fakeSrv("backup", 1, true)
	d := newDispatchDownloader([]*Server{primary, backup})
	ids := make([]string, 10)
	for i := range ids {
		ids[i] = fmt.Sprintf("a%d@h", i)
	}
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, d.dispatcher, j, m)
	d.workCh["primary"] <- &articleRequest{messageID: "blocker"}
	var mask serverMask
	mask.set(0)
	d.tracker.Lock()
	d.tracker.SetTriedLocked(keyFor(j, 7), mask)
	d.tracker.Unlock()

	plan := d.buildDispatchPlan(context.Background(), defaultOpts(d.servers))

	if plan.dispatched != 1 {
		t.Errorf("dispatched = %d, want 1", plan.dispatched)
	}
	select {
	case req := <-d.workCh["backup"]:
		if req.artIdx != 7 {
			t.Errorf("backup received article %d (%s), want 7: only it has been tried on the primary", req.artIdx, req.messageID)
		}
	default:
		t.Error("backup received nothing; the article the primary tried is stuck")
	}
}

// A job whose remaining articles can all only wait is not walked further: with
// none of its try-list entries left to visit, a waiting fresh article ends its
// walk, and an entry keeps the walk going until it is reached. An entry of an
// instance the pass does not walk does not keep it going.
func TestBuildDispatchPlan_StopsOnceNoUnvisitedArticleCanBeDispatched(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		tried       []int32 // articles the primary has already tried
		inFlight    []int32 // of those, the ones a fetch is still running for
		ghost       bool    // an entry for an instance the pass never walks
		primaryRoom int     // free primary slots; 0 means it starts full
		backupCap   int     // backup slots; 0 means 4
		wantScanned int
		wantBackup  []int32
	}{
		{name: "no try-list entries", wantScanned: 0},
		{name: "an entry outside the walk", ghost: true, wantScanned: 0},
		{name: "an entry at article 7", tried: []int32{7}, wantScanned: 8, wantBackup: []int32{7}},
		{name: "an in-flight entry at article 3", tried: []int32{3}, inFlight: []int32{3}, wantScanned: 4},
		// Articles 0 and 1 go to the primary first; neither counts as a visited
		// entry, and both of the job's entries are reached.
		{name: "two entries after fresh dispatches", tried: []int32{5, 8}, primaryRoom: 2, wantScanned: 9, wantBackup: []int32{5, 8}},
		// Article 2 fills the backup, and with every server full the pass ends
		// there rather than walking on to the job's other entry.
		{name: "every server fills mid-walk", tried: []int32{2, 5}, backupCap: 1, wantScanned: 3, wantBackup: []int32{2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			primary := fakeSrv("primary", 0, true)
			backup := fakeSrv("backup", 1, true)
			d := newDispatchDownloader([]*Server{primary, backup})
			d.workCh["backup"] = make(chan *articleRequest, cmp.Or(tc.backupCap, 4))
			j, m := makeJobWithArticles(t, articleIDs(10))
			addTestJob(t, d.dispatcher, j, m)
			if tc.primaryRoom > 0 {
				d.workCh["primary"] = make(chan *articleRequest, tc.primaryRoom)
			} else {
				d.workCh["primary"] <- &articleRequest{messageID: "blocker"}
			}
			var mask serverMask
			mask.set(0)
			d.tracker.Lock()
			for _, idx := range tc.tried {
				d.tracker.SetTriedLocked(keyFor(j, idx), mask)
			}
			for _, idx := range tc.inFlight {
				d.tracker.IncrementInFlightLocked(keyFor(j, idx))
			}
			ghost := bareJob("ghost")
			if tc.ghost {
				d.tracker.SetTriedLocked(keyFor(ghost, 0), mask)
			}
			d.tracker.Unlock()

			plan := d.buildDispatchPlan(context.Background(), defaultOpts(d.servers))

			if plan.scanned != tc.wantScanned {
				t.Errorf("scanned = %d, want %d", plan.scanned, tc.wantScanned)
			}
			var got []int32
			for !drained(d.workCh["backup"]) {
				got = append(got, (<-d.workCh["backup"]).artIdx)
			}
			slices.Sort(got) // drained requeues what it peeks, so order is lost
			if fmt.Sprint(got) != fmt.Sprint(tc.wantBackup) {
				t.Errorf("backup received %v, want %v", got, tc.wantBackup)
			}
		})
	}
}

// A job whose walk ends early does not end the pass: a later job's article
// that needs the backup is still reached.
func TestBuildDispatchPlan_AWaitingJobDoesNotHideALaterJobsBackupArticle(t *testing.T) {
	t.Parallel()

	primary := fakeSrv("primary", 0, true)
	backup := fakeSrv("backup", 1, true)
	d := newDispatchDownloader([]*Server{primary, backup})
	first, fm := makeJobWithArticles(t, articleIDs(5))
	addTestJob(t, d.dispatcher, first, fm)
	second := job.New("j2", "j2.nzb", job.Policy{})
	_, sm := makeJobWithArticles(t, []string{"b0@h", "b1@h", "b2@h"})
	addTestJob(t, d.dispatcher, second, sm)
	d.workCh["primary"] <- &articleRequest{messageID: "blocker"}
	var mask serverMask
	mask.set(0)
	d.tracker.Lock()
	// The first job's walk begins, at its in-flight article 1, and ends at
	// article 2, which waits.
	d.tracker.SetTriedLocked(keyFor(first, 1), mask)
	d.tracker.IncrementInFlightLocked(keyFor(first, 1))
	d.tracker.SetTriedLocked(keyFor(second, 2), mask)
	d.tracker.Unlock()

	d.buildDispatchPlan(context.Background(), defaultOpts(d.servers))

	select {
	case req := <-d.workCh["backup"]:
		if req.job != second || req.artIdx != 2 {
			t.Errorf("backup received %s/%d, want j2/2", req.jobID(), req.artIdx)
		}
	default:
		t.Error("backup received nothing; the first job's waiting articles ended the pass")
	}
}

// Within a tier the first server in slice order with room takes the article.
func TestTryDispatch_TheFirstPeerWithRoomTakesTheArticle(t *testing.T) {
	t.Parallel()

	peerA := fakeSrv("peerA", 0, true)
	peerB := fakeSrv("peerB", 0, true)
	d := newDispatchDownloader([]*Server{peerA, peerB})

	handled, _, _ := d.tryDispatch(context.Background(), fakeArticle("msg1@h"), defaultOpts(d.servers))

	if !handled {
		t.Fatal("article not dispatched")
	}
	if drained(d.workCh["peerA"]) {
		t.Error("peerA, first in slice order, did not receive the article")
	}
}

// An article with no server left is reported however many others wait: the
// stop rule above does not end the pass before it reaches every one of them.
func TestBuildDispatchPlan_ReportsEveryArticleWithNoServerLeft(t *testing.T) {
	t.Parallel()

	d := newDispatchDownloader([]*Server{fakeSrv("off", 0, false)})
	j, m := makeJobWithArticles(t, articleIDs(5))
	addTestJob(t, d.dispatcher, j, m)

	plan := d.buildDispatchPlan(context.Background(), defaultOpts(d.servers))

	if len(plan.exhausted) != 5 {
		t.Errorf("exhausted = %d articles, want 5", len(plan.exhausted))
	}
}

// ---------- end to end, over the mock NNTP server ----------

func newPriorityPair(t *testing.T, primaryAddr, backupAddr string) (*Server, *Server) {
	t.Helper()
	p := testServer(t, "primary", primaryAddr, func(c *config.ServerConfig) {
		c.Priority = 0
		c.Connections = 1
	})
	b := testServer(t, "backup", backupAddr, func(c *config.ServerConfig) {
		c.Priority = 1
		c.Connections = 2
	})
	return p, b
}

func articleIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("art%02d@h", i)
	}
	return ids
}

// A primary with every article and a single slot serves the whole job; the
// backup is never dialled.
func TestDownloader_BackupStaysIdleWhileThePrimaryHasEveryArticle(t *testing.T) {
	ids := articleIDs(30)
	primary := newMockNNTP(t)
	backup := newMockNNTP(t)
	for _, id := range ids {
		body := string(yencBody(id, []byte("body-"+id)))
		primary.addArticle(id, body)
		backup.addArticle(id, body)
	}
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, disp, j, m)

	p, b := newPriorityPair(t, primary.addr, backup.addr)
	d := New(disp, []*Server{p, b}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	for _, r := range collectAcked(t, d, len(ids)) {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.MessageID, r.Err)
		}
		if r.ServerName != "primary" {
			t.Errorf("%s served by %q, want primary", r.MessageID, r.ServerName)
		}
	}
	if got := backup.dials.Load(); got != 0 {
		t.Errorf("backup dialled %d times, want 0", got)
	}
	if got := backup.fetches.Load(); got != 0 {
		t.Errorf("backup served %d articles, want 0", got)
	}
}

// Only the articles the primary answers 430 for reach the backup.
func TestDownloader_OnlyArticlesThePrimaryLacksReachTheBackup(t *testing.T) {
	ids := articleIDs(20)
	missing := map[string]bool{ids[2]: true, ids[9]: true, ids[17]: true}
	primary := newMockNNTP(t)
	backup := newMockNNTP(t)
	for _, id := range ids {
		body := string(yencBody(id, []byte("body-"+id)))
		backup.addArticle(id, body)
		if missing[id] {
			primary.rejectArticle(id)
		} else {
			primary.addArticle(id, body)
		}
	}
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, disp, j, m)

	p, b := newPriorityPair(t, primary.addr, backup.addr)
	d := New(disp, []*Server{p, b}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	for _, r := range collectAcked(t, d, len(ids)) {
		switch {
		case errors.Is(r.Err, nntp.ErrNoArticle):
			if !missing[r.MessageID] || r.ServerName != "primary" {
				t.Errorf("unexpected 430 for %s from %q", r.MessageID, r.ServerName)
			}
		case r.Err != nil:
			t.Errorf("%s: %v", r.MessageID, r.Err)
		case missing[r.MessageID] != (r.ServerName == "backup"):
			t.Errorf("%s served by %q (missing on primary: %v)", r.MessageID, r.ServerName, missing[r.MessageID])
		}
	}
	if got := backup.fetches.Load(); got != int64(len(missing)) {
		t.Errorf("backup served %d articles, want %d", got, len(missing))
	}
}

// A primary penalized mid-download hands the rest of the job to the backup,
// including the articles that were waiting for it when the penalty landed.
func TestDownloader_WorkMovesToTheBackupWhenThePrimaryIsPenalized(t *testing.T) {
	ids := articleIDs(20)
	const failAt = 5
	primary := newMockNNTP(t)
	backup := newMockNNTP(t)
	for _, id := range ids {
		body := string(yencBody(id, []byte("body-"+id)))
		primary.addArticle(id, body)
		backup.addArticle(id, body)
	}
	primary.hangupOnFetch(ids[failAt])
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, disp, j, m)

	p, b := newPriorityPair(t, primary.addr, backup.addr)
	d := New(disp, []*Server{p, b}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	served := make(map[string]string, len(ids))
	for _, r := range collectAcked(t, d, len(ids)) {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.MessageID, r.Err)
		}
		served[r.MessageID] = r.ServerName
	}
	if p.Active(time.Now()) {
		t.Fatal("primary is not penalized; the hang-up did not exercise the penalty path")
	}
	// How many articles the primary served before the hang-up depends on when
	// its slot frees during a pass, so only "some" is asserted: the first
	// pass queues art00 on it ahead of everything else.
	if got := primary.fetches.Load(); got < 1 {
		t.Errorf("primary served %d articles, want the penalty to land mid-download", got)
	}
	if got := primary.fetches.Load() + backup.fetches.Load(); got != int64(len(ids)) {
		t.Errorf("primary and backup served %d articles together, want %d", got, len(ids))
	}
	if got := served[ids[failAt]]; got != "backup" {
		t.Errorf("the article the primary hung up on was served by %q, want backup", got)
	}
}

// collectAcked reads results until n of them are successes, marking each
// success written so the next pass does not offer it again. It returns every
// result read, failures included.
func collectAcked(t *testing.T, d *Downloader, n int) []*ArticleResult {
	t.Helper()
	var out []*ArticleResult
	deadline := time.After(10 * time.Second)
	for ok := 0; ok < n; {
		select {
		case r := <-d.Completions():
			if r.Err == nil {
				ackDone(t, d.dispatcher, r.JobID(), r.MessageID)
				ok++
			}
			out = append(out, r)
		case <-deadline:
			t.Fatalf("timeout after %d of %d successes", ok, n)
		}
	}
	return out
}
