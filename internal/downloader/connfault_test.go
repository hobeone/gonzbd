package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
	"weak"

	"github.com/hobeone/gonzbd/internal/bpsmeter"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nntp"
)

// One article that stalls past the server's timeout must cost that one
// connection and a retry, not the server: with one server configured, a
// server-wide penalty stops the whole queue (#986).
func TestDownloader_StalledArticleDoesNotBenchServer(t *testing.T) {
	ms := newMockNNTP(t)
	ids := []string{"stall@h", "b@h", "c@h", "d@h"}
	for _, id := range ids {
		ms.addArticle(id, string(yencBody(id+".bin", []byte("body-"+id))))
	}
	ms.stallOnFetch("stall@h", 1)

	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, disp, j, m)

	srv := testServer(t, "only", ms.addr, func(c *config.ServerConfig) { c.Timeout = 1 })
	d := New(disp, []*Server{srv}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	// Well under any penalty, well over one 1s timeout and a retry.
	results := collect(t, d.Completions(), len(ids), 20*time.Second)
	seen := make(map[string]int)
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("%s: err = %v, want success after the stall's retry", r.MessageID, r.Err)
		}
		seen[r.MessageID]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("%s: %d results, want exactly 1", id, seen[id])
		}
	}
	if got := ms.stalls.Load(); got != 1 {
		t.Fatalf("mock stalled %d times, want 1: the test did not exercise a timeout", got)
	}
	if pe := srv.PenaltyExpiry(); !pe.IsZero() {
		t.Errorf("PenaltyExpiry = %v, want zero: one timed-out connection penalized the server", pe)
	}
	// The retried article is fetched once more, not reported twice.
	select {
	case r := <-d.Completions():
		t.Errorf("extra result for %s (err %v): the stalled article was counted twice", r.MessageID, r.Err)
	case <-time.After(300 * time.Millisecond):
	}
}

// A server that loses every connection mid-command is still penalized, so its
// work moves to the next server, and it is redialled a bounded number of times
// first rather than hot-looping.
func TestDownloader_ServerLosingEveryConnectionIsPenalized(t *testing.T) {
	const conns = 2
	ids := []string{"a@h", "b@h", "c@h"}

	dead := newMockNNTP(t)
	backup := newMockNNTP(t)
	for _, id := range ids {
		dead.addArticle(id, "unused")
		dead.hangupOnFetch(id)
		backup.addArticle(id, string(yencBody(id+".bin", []byte("body-"+id))))
	}

	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, disp, j, m)

	withConns := func(c *config.ServerConfig) { c.Connections = conns }
	srvDead := testServer(t, "dead", dead.addr, withConns)
	srvBackup := testServer(t, "backup", backup.addr, withConns, func(c *config.ServerConfig) { c.Priority = 1 })
	d := New(disp, []*Server{srvDead, srvBackup}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	results := collect(t, d.Completions(), len(ids), 10*time.Second)
	for _, r := range results {
		if r.Err != nil || r.ServerName != "backup" {
			t.Errorf("%s: server %q err %v, want success from backup", r.MessageID, r.ServerName, r.Err)
		}
	}
	if srvDead.PenaltyExpiry().IsZero() {
		t.Error("the server that lost every connection was never penalized")
	}
	// conns+1 faults earn the penalty, each costing one dial, and each of
	// the conns workers may have one more dial past the Active check; the
	// limit rounds that 2*conns+1 up.
	if got, limit := dead.dials.Load(), int64(2*(conns+1)); got > limit {
		t.Errorf("dead server dialled %d times, want <= %d", got, limit)
	}
}

// A run of connection faults earns a penalty only once it exceeds the
// server's connection count, and the penalty is clamped like any other.
func TestFetchArticle_ConnFaultRunAppliesClampedPenalty(t *testing.T) {
	t.Parallel()

	ms := newMockNNTP(t)
	ms.addArticle("hangup@h", "body")
	ms.hangupOnFetch("hangup@h")

	srv := testServer(t, "s", ms.addr, func(c *config.ServerConfig) { c.Connections = 1 })
	d := &Downloader{
		dispatcher: newTestDispatcher(t),
		tracker:    newDispatchTracker(),
		log:        slog.New(slog.DiscardHandler),
		opts:       Options{NoPenalties: true},
		limiter:    bpsmeter.NewLimiter(0),
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	j, m := makeJobWithArticles(t, []string{"hangup@h"})
	addTestJob(t, d.dispatcher, j, m)

	req := &articleRequest{job: j, messageID: "hangup@h"}
	mc := &managedConn{}
	defer mc.Close(d, "worker1")

	if _, ok := d.fetchArticle(t.Context(), srv, 0, mc, req, "worker1"); ok {
		t.Fatal("first fetch: want a connection-level failure")
	}
	if pe := srv.PenaltyExpiry(); !pe.IsZero() {
		t.Fatalf("PenaltyExpiry = %v after one lost connection, want zero", pe)
	}
	if got := srv.BadConnections(); got != 1 {
		t.Errorf("BadConnections = %d after one lost connection, want 1", got)
	}

	before := time.Now()
	if _, ok := d.fetchArticle(t.Context(), srv, 0, mc, req, "worker1"); ok {
		t.Fatal("second fetch: want a connection-level failure")
	}
	got := srv.PenaltyExpiry().Sub(before)
	if got <= 0 {
		t.Fatalf("penalty = %v after more lost connections than the server has, want > 0", got)
	}
	if got > constants.PenaltyShort+2*time.Second {
		t.Errorf("penalty = %v, want <= constants.PenaltyShort (%v) when NoPenalties is set", got, constants.PenaltyShort)
	}
}

// An error the server answered with is the server's fault and is penalized at
// once, unlike a lost connection.
func TestFetchArticle_ServerResponseFaultPenalizesAtOnce(t *testing.T) {
	t.Parallel()

	ms := newMockNNTP(t)
	ms.respondOnFetch("busy@h", "502 service unavailable")

	srv := testServer(t, "s", ms.addr)
	d := &Downloader{
		dispatcher: newTestDispatcher(t),
		tracker:    newDispatchTracker(),
		log:        slog.New(slog.DiscardHandler),
		limiter:    bpsmeter.NewLimiter(0),
	}
	d.pauseCtx, d.pauseCancel = context.WithCancel(context.Background())
	defer d.pauseCancel()

	j, m := makeJobWithArticles(t, []string{"busy@h"})
	addTestJob(t, d.dispatcher, j, m)

	mc := &managedConn{}
	defer mc.Close(d, "worker1")
	before := time.Now()
	if _, ok := d.fetchArticle(t.Context(), srv, 0, mc, &articleRequest{job: j, messageID: "busy@h"}, "worker1"); ok {
		t.Fatal("want a failed fetch")
	}
	if got := srv.PenaltyExpiry().Sub(before); got < constants.Penalty502-2*time.Second {
		t.Errorf("penalty = %v, want about constants.Penalty502 (%v) after one 502", got, constants.Penalty502)
	}
}

func TestServer_RecordConnFault(t *testing.T) {
	t.Parallel()
	s := checkedServer(config.ServerConfig{
		Name: "s", Host: "h", Port: 119, Connections: 2, PipeliningRequests: 1, Timeout: 5, Enable: true,
	})

	for i, want := range []bool{false, false, true, false, false, true} {
		if got := s.RecordConnFault(); got != want {
			t.Fatalf("fault %d: RecordConnFault() = %v, want %v", i+1, got, want)
		}
	}
	if got := s.BadConnections(); got != 0 {
		t.Errorf("BadConnections = %d after a reported run, want 0", got)
	}

	s.RecordConnFault()
	s.RecordConnFault()
	s.RecordGoodConnection()
	if s.RecordConnFault() {
		t.Error("a success between faults did not restart the run")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

func TestIsConnFault(t *testing.T) {
	t.Parallel()
	opErr := func(e error) error { return &net.OpError{Op: "read", Net: "tcp", Err: e} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"read deadline", opErr(os.ErrDeadlineExceeded), true},
		{"timeout type", fmt.Errorf("wrapped: %w", timeoutErr{}), true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"reset", opErr(&os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}), true},
		{"aborted", opErr(syscall.ECONNABORTED), true},
		{"broken pipe on write", fmt.Errorf("nntp: write: %w", opErr(syscall.EPIPE)), true},
		{"502", fmt.Errorf("%w: 502 busy", nntp.ErrServerUnavailable), false},
		{"desync", nntp.ErrDesynced, false},
		{"refused", opErr(syscall.ECONNREFUSED), false},
		{"protocol", errors.New("nntp: malformed response"), false},
	}
	for _, tc := range cases {
		if got := isConnFault(tc.err); got != tc.want {
			t.Errorf("%s: isConnFault(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// An article that loses its connection every time on a server that otherwise
// works moves to the next server after maxConnFaultsPerArticle faults, without
// benching the server it left.
func TestDownloader_ArticleFaultingEveryTimeMovesToNextServer(t *testing.T) {
	ids := make([]string, 1, 9)
	ids[0] = "poison@h"
	for i := range 8 {
		ids = append(ids, fmt.Sprintf("ok%d@h", i))
	}

	primary := newMockNNTP(t)
	backup := newMockNNTP(t)
	for _, id := range ids {
		body := string(yencBody(id+".bin", []byte("body-"+id)))
		primary.addArticle(id, body)
		backup.addArticle(id, body)
	}
	primary.hangupOnFetch("poison@h")

	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, ids)
	addTestJob(t, disp, j, m)

	// Enough connections that a run of the poison article's faults alone
	// cannot reach the server-wide threshold first.
	srvPrimary := testServer(t, "primary", primary.addr, func(c *config.ServerConfig) { c.Connections = 8 })
	srvBackup := testServer(t, "backup", backup.addr, func(c *config.ServerConfig) { c.Priority = 1 })
	d := New(disp, []*Server{srvPrimary, srvBackup}, nil, Options{}, nil)
	if err := d.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	results := collect(t, d.Completions(), len(ids), 10*time.Second)
	for _, r := range results {
		want := "primary"
		if r.MessageID == "poison@h" {
			want = "backup"
		}
		if r.Err != nil || r.ServerName != want {
			t.Errorf("%s: server %q err %v, want success from %s", r.MessageID, r.ServerName, r.Err, want)
		}
	}
	if pe := srvPrimary.PenaltyExpiry(); !pe.IsZero() {
		t.Errorf("primary PenaltyExpiry = %v, want zero: one bad article benched its server", pe)
	}
}

func TestTracker_UnmarkTriedAfterFault(t *testing.T) {
	t.Parallel()
	tr := newDispatchTracker()
	key := keyFor(bareJob("jf"), 1)
	tried := func() bool {
		tr.Lock()
		defer tr.Unlock()
		mask, _ := tr.TryListLocked(key)
		return mask.has(0)
	}
	mark := func() {
		tr.Lock()
		defer tr.Unlock()
		var mask serverMask
		mask.set(0)
		tr.SetTriedLocked(key, mask)
	}

	for round := range 2 {
		for i := 1; i <= maxConnFaultsPerArticle; i++ {
			mark()
			kept := tr.UnmarkTriedAfterFault(key, 0)
			if want := i == maxConnFaultsPerArticle; kept != want || tried() != want {
				t.Fatalf("round %d fault %d: kept=%v tried=%v, want both %v", round, i, kept, tried(), want)
			}
		}
	}

	mark()
	tr.UnmarkTriedAfterFault(key, 0)
	tr.ClearTried(key)
	for i := 1; i < maxConnFaultsPerArticle; i++ {
		mark()
		if tr.UnmarkTriedAfterFault(key, 0) {
			t.Fatalf("fault %d after ClearTried left the server marked: the count survived", i)
		}
	}
	if refs := tr.Instances(); len(refs) != 1 {
		t.Fatalf("Instances = %d, want the instance holding a fault count", len(refs))
	}
	tr.DropInstances(map[weak.Pointer[job.Job]]struct{}{key.inst: {}})
	if len(tr.faults) != 0 {
		t.Errorf("faults = %v after DropInstances, want none", tr.faults)
	}
}
