//go:build crash && linux

// Package crash runs the real gonzbd binary as a child process and kills it,
// so that the durability design's central claim is measured rather than
// assumed.
//
// # What a SIGKILL here actually destroys, and what it does not
//
// This distinction decides what every test in this package is entitled to
// conclude, so it is stated before anything else.
//
// DESTROYED FOR REAL: everything the process held in its own memory. That is
// the decoded articles queued ahead of the assembler, the
// downloader's in-flight buffers, the recorder's unflushed rows, and the
// queue's unsaved in-memory state. A SIGKILL gives the process no chance to
// flush any of it, so an article recorded written before its bytes left the
// process is an article whose bytes are simply absent from the file
// afterwards. That is the over-claim these tests catch, and they catch it with
// no simulation of any kind.
//
// NOT DESTROYED: the kernel page cache. A write(2) that returned before the
// kill is visible to every later reader whether or not it was ever fsynced,
// and no unprivileged interface can throw it away. POSIX_FADV_DONTNEED
// invalidates CLEAN pages only, and /proc/sys/vm/drop_caches likewise skips
// dirty ones — neither is a way to lose unfsynced data. So this package does
// NOT test that an fsync'd byte reached the platter, and it must not be read
// as doing so. That half needs a device the test can cut underneath the
// filesystem (a device-mapper log-writes or flakey target), which needs root.
//
// dropPageCache below is therefore honestly named for what it does: it
// evicts the clean pages, which forces the read-back of already-written-back
// ranges to come from the block device instead of from cache. That is a real
// strengthening of the read-back and not a power-loss simulation, and it is
// documented as such at the call site rather than in the test names.
package crash

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // read-only access to the daemon's own database

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

// binPath is the gonzbd binary built once for the whole package by TestMain.
var binPath string

// fileSpec describes one file the synthetic NZB carries.
type fileSpec struct {
	Name     string
	Size     int
	PartSize int
}

// harnessOpts are the knobs a test varies. Zero values take the defaults
// applied in newHarness.
type harnessOpts struct {
	// CheckpointBytes and CheckpointInterval are B1's two bounds, written
	// straight into the daemon's config file.
	CheckpointBytes    int64
	CheckpointInterval time.Duration
	// Connections is the NNTP connection count, which bounds how many
	// articles can be in flight and therefore also feeds the rework bound.
	Connections int
	// BodyDelay throttles the mock server so a job takes long enough to be
	// killed part-way through. Without it a loopback download of any size
	// this suite can afford finishes before the first record flush.
	BodyDelay time.Duration
	// Files defaults to a single multi-part file.
	Files []fileSpec
}

// harness owns one daemon instance's directories, config, mock server and
// child process across as many starts and kills as a test needs.
type harness struct {
	t    *testing.T
	opts harnessOpts

	Dir         string
	DownloadDir string
	CompleteDir string
	AdminDir    string
	ConfigPath  string
	NZBPath     string
	DBPath      string

	APIKey  string
	BaseURL string
	port    int

	Server *mocknntp.Server
	// Payloads is the expected content of each file, by fileSpec index.
	Payloads [][]byte
	// MsgIDs[fileIdx][partIdx] is the Message-ID of one article, so a test
	// can turn the mock server's per-ID delivery counts into per-article
	// facts.
	MsgIDs [][]string
	// ArticleSize is the raw payload bytes per article part.
	ArticleSize int

	mu      sync.Mutex
	cmd     *exec.Cmd
	logFile *os.File
	logPath string
}

// newHarness prepares the directories, the fixture and the config, starts the
// mock server, and starts the daemon. It does not add a job.
func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	if binPath == "" {
		t.Fatal("newHarness: the gonzbd binary was not built; TestMain did not run")
	}
	if opts.CheckpointBytes == 0 {
		opts.CheckpointBytes = 1 << 20
	}
	if opts.CheckpointInterval == 0 {
		opts.CheckpointInterval = time.Hour
	}
	if opts.Connections == 0 {
		opts.Connections = 1
	}
	if len(opts.Files) == 0 {
		opts.Files = []fileSpec{{Name: "payload.bin", Size: 8 << 20, PartSize: 128 << 10}}
	}

	h := &harness{t: t, opts: opts, Dir: t.TempDir()}
	h.DownloadDir = filepath.Join(h.Dir, "incomplete")
	h.CompleteDir = filepath.Join(h.Dir, "complete")
	h.AdminDir = filepath.Join(h.Dir, "admin")
	h.ConfigPath = filepath.Join(h.Dir, "gonzbd.yaml")
	h.NZBPath = filepath.Join(h.Dir, "job.nzb")
	h.DBPath = filepath.Join(h.AdminDir, "history.db")
	h.logPath = filepath.Join(h.Dir, "gonzbd.log")
	for _, d := range []string{h.DownloadDir, h.CompleteDir, h.AdminDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	h.ArticleSize = opts.Files[0].PartSize

	h.startMockServer()
	h.buildFixture()
	h.writeConfig()
	h.Start()
	t.Cleanup(h.Stop)
	return h
}

// startMockServer brings up the in-process NNTP endpoint the child connects
// back to over loopback. It lives in the TEST process deliberately: the child
// is the thing being killed, and a server that died with it could not report
// what the restarted child re-fetched.
func (h *harness) startMockServer() {
	h.Server = mocknntp.NewServer(mocknntp.Config{BodyDelay: h.opts.BodyDelay})
	if err := h.Server.Start(); err != nil {
		h.t.Fatalf("mocknntp start: %v", err)
	}
	h.t.Cleanup(func() { _ = h.Server.Close() })
}

// buildFixture generates deterministic payloads, registers every article with
// the mock server, and writes the NZB.
func (h *harness) buildFixture() {
	files := make([]nzbFile, len(h.opts.Files))
	for i, f := range h.opts.Files {
		payload := deterministicPayload(f.Name, f.Size)
		h.Payloads = append(h.Payloads, payload)
		parts := splitParts(payload, f.PartSize)
		ids := make([]string, len(parts))
		for p, part := range parts {
			id, body := buildArticle(part, f.Name, p+1, len(parts), int64(len(payload)),
				int64(p*f.PartSize))
			h.Server.AddArticle(id, body)
			ids[p] = id
		}
		h.MsgIDs = append(h.MsgIDs, ids)
		files[i] = nzbFile{Name: f.Name, Parts: parts, IDs: ids}
	}
	if err := os.WriteFile(h.NZBPath, buildNZB(files), 0o600); err != nil {
		h.t.Fatalf("write nzb: %v", err)
	}
}

// writeConfig renders the daemon's YAML from the real config defaults, so the
// harness cannot drift from the schema the daemon validates.
func (h *harness) writeConfig() {
	cfg, err := config.Default()
	if err != nil {
		h.t.Fatalf("config.Default: %v", err)
	}
	host, portStr, err := net.SplitHostPort(h.Server.Addr())
	if err != nil {
		h.t.Fatalf("split mock addr: %v", err)
	}
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		h.t.Fatalf("parse mock port: %v", err)
	}
	h.port = freePort(h.t)
	cfg.With(func(c *config.Config) {
		c.General.Host = "127.0.0.1"
		c.General.Port = h.port
		c.General.DownloadDir = h.DownloadDir
		c.General.CompleteDir = h.CompleteDir
		c.General.AdminDir = h.AdminDir
		c.General.LogDir = h.Dir
		c.General.ScriptDir = ""
		c.Downloads.CheckpointBytes = config.ByteSize(h.opts.CheckpointBytes)
		c.Downloads.CheckpointInterval = int(h.opts.CheckpointInterval.Seconds())
		c.Downloads.MaxArtTries = 3
		c.Servers = []config.ServerConfig{{
			Name:               "mock",
			Host:               host,
			Port:               port,
			Connections:        h.opts.Connections,
			Enable:             true,
			Timeout:            30,
			PipeliningRequests: 1,
			Priority:           0,
			Retention:          0,
		}}
	})
	h.APIKey = cfg.GetGeneral().APIKey
	h.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", h.port)
	if err := cfg.Save(h.ConfigPath); err != nil {
		h.t.Fatalf("config.Save: %v", err)
	}
}

// Start launches the daemon and blocks until its API answers.
func (h *harness) Start() {
	h.t.Helper()
	f, err := os.OpenFile(h.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		h.t.Fatalf("open daemon log: %v", err)
	}
	cmd := exec.Command(binPath, "--config", h.ConfigPath, "--serve") //nolint:gosec // binPath is built by TestMain
	cmd.Stdout = f
	cmd.Stderr = f
	// Its own process group, so a kill cannot reach the test runner.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		h.t.Fatalf("start daemon: %v", err)
	}
	h.mu.Lock()
	h.cmd, h.logFile = cmd, f
	h.mu.Unlock()
	h.waitForAPI()
}

// Restart starts a fresh daemon over the same directories.
func (h *harness) Restart() {
	h.t.Helper()
	h.Start()
}

// waitForAPI blocks until mode=version answers, or fails the test.
func (h *harness) waitForAPI() {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.BaseURL + "/api?mode=version&output=json&apikey=" + h.APIKey) //nolint:noctx // short-lived readiness poll
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if !h.running() {
			tail := h.tailLog()
			if strings.Contains(tail, "bind: address already in use") {
				h.t.Fatalf("daemon failed to bind port %d (address already in use):\n%s", h.port, tail)
			}
			h.t.Fatalf("daemon exited before its API came up\n%s", tail)
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("daemon API did not come up within 30s\n%s", h.tailLog())
}

// running reports whether the child is still alive.
func (h *harness) running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd == nil || h.cmd.Process == nil {
		return false
	}
	return h.cmd.ProcessState == nil && h.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// Kill SIGKILLs the daemon and waits for it to be reaped.
//
// SIGKILL, not SIGTERM: a graceful stop flushes the recorder, which records
// everything written and would make every test here measure a clean restart
// instead of a crash.
func (h *harness) Kill() {
	h.t.Helper()
	h.mu.Lock()
	cmd, f := h.cmd, h.logFile
	h.cmd, h.logFile = nil, nil
	h.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		h.t.Fatalf("SIGKILL the daemon: %v", err)
	}
	_ = cmd.Wait()
	if f != nil {
		_ = f.Close()
	}
}

// Stop ends the daemon gracefully, waiting for it to exit.
func (h *harness) Stop() {
	h.mu.Lock()
	cmd, f := h.cmd, h.logFile
	h.cmd, h.logFile = nil, nil
	h.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	if f != nil {
		_ = f.Close()
	}
}

// KillAndDropPageCache SIGKILLs the daemon and then evicts what can be
// evicted from the page cache for every file under the download directory.
//
// The two halves have very different strength, and conflating them is the
// failure mode this comment exists to prevent:
//
//   - The kill is the real one. Everything the daemon held in user space —
//     above all the decoded articles queued ahead of the assembler — is gone,
//     with no flush. An article recorded before its bytes left the process has
//     no bytes on disk afterwards, and the CRC read-back sees exactly that.
//
//   - The eviction is best-effort and partial BY CONSTRUCTION.
//     POSIX_FADV_DONTNEED invalidates clean pages and skips dirty ones, so it
//     forces already-written-back ranges to be re-read from the block device
//     while leaving not-yet-written-back ranges served from cache. It is a
//     strengthening of the read-back, not a simulation of power loss, and no
//     unprivileged call can be one — /proc/sys/vm/drop_caches skips dirty
//     pages too.
//
// A failed fadvise is fatal rather than skipped: a harness that quietly
// degraded to "we read it all back out of cache" would still pass, which is
// the green-gate-that-bounds-nothing shape AGENTS.md warns about.
func (h *harness) KillAndDropPageCache() {
	h.t.Helper()
	h.Kill()
	h.dropPageCache()
}

// dropPageCache fadvises DONTNEED over every regular file under the download
// directory. See KillAndDropPageCache for what that does and does not evict.
func (h *harness) dropPageCache() {
	h.t.Helper()
	n := 0
	err := filepath.WalkDir(h.DownloadDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		f, oerr := os.Open(path) //nolint:gosec // path comes from walking the harness's own temp dir
		if oerr != nil {
			return fmt.Errorf("open %s for fadvise: %w", path, oerr)
		}
		defer func() { _ = f.Close() }()
		fd := int(f.Fd()) //nolint:gosec // G115: a file descriptor is a small non-negative int
		if aerr := unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED); aerr != nil {
			return fmt.Errorf("fadvise(DONTNEED) %s: %w", path, aerr)
		}
		n++
		return nil
	})
	if err != nil {
		h.t.Fatalf("dropping the page cache failed; the read-back below would have "+
			"come from cache and proved nothing: %v", err)
	}
	if n == 0 {
		h.t.Fatal("no file under the download directory to evict — the fixture never " +
			"wrote anything, so a read-back would be vacuous")
	}
}

// tailLog returns the last few KiB of the daemon's output for a failure
// message.
func (h *harness) tailLog() string {
	b, err := os.ReadFile(h.logPath) //nolint:gosec // harness-owned temp path
	if err != nil {
		return "(no daemon log: " + err.Error() + ")"
	}
	const keep = 8 << 10
	if len(b) > keep {
		b = b[len(b)-keep:]
	}
	return "--- daemon log tail ---\n" + string(b)
}

// ---------- API ----------

// api issues one API request and returns the decoded JSON body.
func (h *harness) api(mode string, params url.Values) map[string]any {
	h.t.Helper()
	if params == nil {
		params = url.Values{}
	}
	params.Set("mode", mode)
	params.Set("output", "json")
	params.Set("apikey", h.APIKey)
	resp, err := http.Get(h.BaseURL + "/api?" + params.Encode()) //nolint:noctx,gosec // loopback test client
	if err != nil {
		h.t.Fatalf("api %s: %v", mode, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("api %s: read body: %v", mode, err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("api %s: status %d: %s", mode, resp.StatusCode, body)
	}
	return decodeJSON(h.t, body)
}

// AddJob uploads the fixture NZB and returns the job's ID.
func (h *harness) AddJob() string {
	h.t.Helper()
	nzbBytes, err := os.ReadFile(h.NZBPath)
	if err != nil {
		h.t.Fatalf("read nzb: %v", err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("nzbfile", filepath.Base(h.NZBPath))
	if err != nil {
		h.t.Fatalf("multipart: %v", err)
	}
	if _, err := part.Write(nzbBytes); err != nil {
		h.t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		h.t.Fatalf("multipart close: %v", err)
	}

	u := fmt.Sprintf("%s/api?mode=addfile&output=json&apikey=%s", h.BaseURL, h.APIKey)
	resp, err := http.Post(u, mw.FormDataContentType(), &buf) //nolint:noctx,gosec // loopback test client
	if err != nil {
		h.t.Fatalf("addfile: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("addfile: status %d: %s", resp.StatusCode, body)
	}
	m := decodeJSON(h.t, body)
	ids, _ := m["nzo_ids"].([]any)
	if len(ids) != 1 {
		h.t.Fatalf("addfile returned %v job ids, want exactly 1: %s", len(ids), body)
	}
	id, _ := ids[0].(string)
	if id == "" {
		h.t.Fatalf("addfile returned an empty job id: %s", body)
	}
	return id
}

// slot is the subset of the queue listing these tests read.
type slot struct {
	ID                string
	Status            string
	StallReason       string
	BytesDurable      int64
	ArticlesRemaining int
	MB                float64
	MBLeft            float64
}

// Slot returns the queue row for jobID, or ok=false when the job is no longer
// in the queue (it finished, or it never arrived).
func (h *harness) Slot(jobID string) (slot, bool) {
	h.t.Helper()
	m := h.api("queue", nil)
	q, _ := m["queue"].(map[string]any)
	raw, _ := q["slots"].([]any)
	for _, r := range raw {
		s, _ := r.(map[string]any)
		if str(s["nzo_id"]) != jobID {
			continue
		}
		return slot{
			ID:                jobID,
			Status:            str(s["status"]),
			StallReason:       str(s["stall_reason"]),
			BytesDurable:      i64(s["bytes_durable"]),
			ArticlesRemaining: int(i64(s["articles_remaining"])),
			MB:                f64(s["mb"]),
			MBLeft:            f64(s["mbleft"]),
		}, true
	}
	return slot{}, false
}

// WaitForWrittenBytes blocks until the job reports at least want bytes of
// articles written, and returns the slot it saw.
func (h *harness) WaitForWrittenBytes(jobID string, want int64) slot {
	h.t.Helper()
	var last slot
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		s, ok := h.Slot(jobID)
		if ok {
			last = s
			if s.BytesDurable >= want {
				return s
			}
		}
		if !h.running() {
			h.t.Fatalf("daemon exited while waiting for %d written bytes\n%s", want, h.tailLog())
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("job %s reached only %d written bytes in 90s, want %d (last slot %+v)\n%s",
		jobID, last.BytesDurable, want, last, h.tailLog())
	return slot{}
}

// WaitForRecordedBacklog blocks until written_articles holds at least minRows
// rows for the job AND the mock server has served at least minUnrecorded more
// articles than that, so that a kill issued straight afterwards lands with
// recorded work to keep and unrecorded work at risk.
//
// It reads the database while the daemon runs, which openDB refuses. That is
// sound for a wait: SQLite serves a reader a consistent snapshot, and nothing
// asserted later depends on what this read saw.
func (h *harness) WaitForRecordedBacklog(jobID string, minRows, minUnrecorded int) {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.DBPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		h.t.Fatalf("open %s read-only: %v", h.DBPath, err)
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(90 * time.Second)
	var served, rows int
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT COUNT(*) FROM written_articles WHERE job_id = ?`, jobID).Scan(&rows); err != nil {
			rows = 0
		}
		served = len(h.Server.ArticlesServed())
		if rows >= minRows && served-rows >= minUnrecorded {
			return
		}
		if !h.running() {
			h.t.Fatalf("daemon exited while waiting for a recorded backlog\n%s", h.tailLog())
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("job %s never had %d rows recorded with %d articles unrecorded (last: %d "+
		"served, %d recorded); a kill now would risk nothing or keep nothing\n%s",
		jobID, minRows, minUnrecorded, served, rows, h.tailLog())
}

// WaitForJobFinished blocks until the daemon says the job is done: it has
// left the queue and every fixture file EXISTS under the complete directory.
//
// Deliberately separate from the byte check below. "The daemon declared this
// finished" and "the bytes are right" are different claims, and a harness that
// waits on the second can only ever report a timeout when the first is true
// and the second is false — which is the most interesting outcome there is.
func (h *harness) WaitForJobFinished(jobID string) {
	h.t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, ok := h.Slot(jobID); !ok {
			lastErr = h.completedFilesExist()
			if lastErr == nil {
				return
			}
		} else {
			lastErr = errors.New("still in the queue")
		}
		if !h.running() {
			h.t.Fatalf("daemon exited before job %s finished: %v\n%s", jobID, lastErr, h.tailLog())
		}
		time.Sleep(100 * time.Millisecond)
	}
	s, _ := h.Slot(jobID)
	h.t.Fatalf("job %s did not finish within 120s: %v (last slot %+v)\n%s",
		jobID, lastErr, s, h.tailLog())
}

// AssertCompletedFilesMatch compares each completed file against the bytes the
// mock server actually served.
func (h *harness) AssertCompletedFilesMatch() {
	h.t.Helper()
	for i, spec := range h.opts.Files {
		path, err := h.findUnder(h.CompleteDir, spec.Name)
		if err != nil {
			h.t.Fatalf("locate the completed file: %v", err)
		}
		got, err := os.ReadFile(path) //nolint:gosec // harness-owned temp path
		if err != nil {
			h.t.Fatalf("read the completed file: %v", err)
		}
		if !bytes.Equal(got, h.Payloads[i]) {
			h.t.Errorf("%s is %d bytes (want %d) and its content is wrong: first difference at %s",
				path, len(got), len(h.Payloads[i]), describeDiff(got, h.Payloads[i], h.ArticleSize))
		}
	}
}

// WaitForJobComplete waits for the daemon to finish the job and then checks
// the bytes.
func (h *harness) WaitForJobComplete(jobID string) {
	h.t.Helper()
	h.WaitForJobFinished(jobID)
	h.AssertCompletedFilesMatch()
}

// completedFilesExist reports nil once every fixture file is present under the
// complete directory, whatever its content.
func (h *harness) completedFilesExist() error {
	for _, spec := range h.opts.Files {
		if _, err := h.findUnder(h.CompleteDir, spec.Name); err != nil {
			return err
		}
	}
	return nil
}

// findUnder returns the single path under root whose base name is name.
func (h *harness) findUnder(root, name string) (string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no file named %q under %s", name, root)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("%d files named %q under %s: %v", len(found), name, root, found)
	}
}

// CompletedJobFileNames returns the base names of the regular files in the
// job's directory after it has been moved to the complete directory.
//
// The whole job directory is renamed into place by post-processing, so a file
// the daemon orphaned during the download travels with it and is visible here.
// That is what makes this the deterministic place to check for one: it needs no
// race against the job finishing, unlike sampling the live partial set.
func (h *harness) CompletedJobFileNames() []string {
	h.t.Helper()
	var out []string
	err := filepath.WalkDir(h.CompleteDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		h.t.Fatalf("walk the complete directory: %v", err)
	}
	slices.Sort(out)
	return out
}

// JobDir returns the job's working directory under the download directory.
func (h *harness) JobDir() string {
	h.t.Helper()
	entries, err := os.ReadDir(h.DownloadDir)
	if err != nil {
		h.t.Fatalf("read download dir: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(h.DownloadDir, e.Name()))
		}
	}
	if len(dirs) != 1 {
		h.t.Fatalf("expected exactly one job directory under %s, found %v", h.DownloadDir, dirs)
	}
	return dirs[0]
}

// PartialPaths lists the regular files in the job's working directory.
func (h *harness) PartialPaths() []string {
	h.t.Helper()
	entries, err := os.ReadDir(h.JobDir())
	if err != nil {
		h.t.Fatalf("read job dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, filepath.Join(h.JobDir(), e.Name()))
		}
	}
	return out
}

// ---------- stable-storage readers ----------
//
// Everything below reads the daemon's own database directly, with the daemon
// dead. That is deliberate: after a SIGKILL the database is the only record
// of what the system CLAIMED, and the files are the only record of what it
// actually has. Asking the running daemon would be asking the accused.

// openDB opens the daemon's database. The caller must have killed the daemon
// first.
func (h *harness) openDB() *sql.DB {
	h.t.Helper()
	if h.running() {
		h.t.Fatal("openDB called while the daemon is running; these readers describe " +
			"stable storage and mean nothing against a live writer")
	}
	db, err := sql.Open("sqlite", h.DBPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		h.t.Fatalf("open %s: %v", h.DBPath, err)
	}
	h.t.Cleanup(func() { _ = db.Close() })
	return db
}

// writtenRow mirrors one written_articles row: an article whose decoded bytes
// were written at [Offset, Offset+Length), with their CRC32.
type writtenRow struct {
	FileIdx int32
	ArtIdx  int32
	Offset  int64
	Length  int64
	CRC32   uint32
}

// Rows reads every written_articles row for a job from stable storage, keyed
// by file index and ordered by offset within each file.
func (h *harness) Rows(db *sql.DB, jobID string) map[int32][]writtenRow {
	h.t.Helper()
	rows, err := db.Query(
		`SELECT file_idx, art_idx, offset, length, crc32
		   FROM written_articles WHERE job_id = ? ORDER BY file_idx, offset`, jobID)
	if err != nil {
		h.t.Fatalf("query written_articles: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int32][]writtenRow{}
	for rows.Next() {
		var r writtenRow
		if err := rows.Scan(&r.FileIdx, &r.ArtIdx, &r.Offset, &r.Length, &r.CRC32); err != nil {
			h.t.Fatalf("scan written_articles: %v", err)
		}
		out[r.FileIdx] = append(out[r.FileIdx], r)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("written_articles rows: %v", err)
	}
	return out
}

// jobFile mirrors the job_files columns these tests read, plus the article
// count.
//
// ArticleCount does NOT come from the database. It is len(h.MsgIDs[fileIdx]),
// taken from the fixture this harness built and served, so the global article
// indices in written_articles are turned into file-local ordinals against
// what was submitted rather than against what the daemon stored.
type jobFile struct {
	FileIdx      int32
	Filename     string
	ArticleCount int
	Complete     bool
}

// JobFiles reads the queue's own per-file state from stable storage.
func (h *harness) JobFiles(db *sql.DB, jobID string) []jobFile {
	h.t.Helper()
	rows, err := db.Query(
		`SELECT file_index, COALESCE(filename,''), complete
		   FROM job_files WHERE job_id = ? ORDER BY file_index`, jobID)
	if err != nil {
		h.t.Fatalf("query job_files: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []jobFile
	for rows.Next() {
		var jf jobFile
		var complete int
		if err := rows.Scan(&jf.FileIdx, &jf.Filename, &complete); err != nil {
			h.t.Fatalf("scan job_files: %v", err)
		}
		jf.Complete = complete != 0
		idx := int(jf.FileIdx)
		if idx < 0 || idx >= len(h.MsgIDs) {
			h.t.Fatalf("job_files row names file_index %d, but the fixture built %d "+
				"files; the daemon invented a file the test never submitted",
				jf.FileIdx, len(h.MsgIDs))
		}
		jf.ArticleCount = len(h.MsgIDs[idx])
		out = append(out, jf)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("job_files rows: %v", err)
	}
	// One row per submitted file, at that file's own index: FileRanges sums
	// ArticleCount in row order, so a gap would shift every later file's base.
	if len(out) != len(h.MsgIDs) {
		h.t.Fatalf("job_files has %d rows, but the fixture submitted %d files",
			len(out), len(h.MsgIDs))
	}
	for i, jf := range out {
		if int(jf.FileIdx) != i {
			h.t.Fatalf("job_files row %d names file_index %d; the rows are ordered by "+
				"file_index, so this is a gap or a duplicate", i, jf.FileIdx)
		}
	}
	return out
}

// RecordedOrdinals returns, per file index, one bool per FILE-LOCAL article
// ordinal: true where written_articles holds a row for that article.
//
// The daemon must already be stopped; see openDB.
func (h *harness) RecordedOrdinals(jobID string) map[int32][]bool {
	h.t.Helper()
	db := h.openDB()
	files := h.JobFiles(db, jobID)
	rows := h.Rows(db, jobID)
	bases := FileRanges(files)
	out := map[int32][]bool{}
	for _, f := range files {
		bits := make([]bool, f.ArticleCount)
		base := bases[f.FileIdx]
		for _, r := range rows[f.FileIdx] {
			if ord := int(r.ArtIdx - base); ord >= 0 && ord < f.ArticleCount {
				bits[ord] = true
			}
		}
		out[f.FileIdx] = bits
	}
	return out
}

// RecordedMessageIDs returns the Message-ID of every article written_articles
// holds a row for.
//
// This — not the set of articles the mock server delivered — is what "must not
// be fetched again" is a claim about. An article can be served and never
// written: the download stops, the article is discarded in flight, and the
// design's answer for it is a re-fetch.
//
// The daemon must already be stopped; see openDB.
func (h *harness) RecordedMessageIDs(jobID string) map[string]bool {
	h.t.Helper()
	out := map[string]bool{}
	for fileIdx, bits := range h.RecordedOrdinals(jobID) {
		for i, recorded := range bits {
			if recorded {
				out[h.MsgIDs[fileIdx][i]] = true
			}
		}
	}
	return out
}

// FileRanges maps each file index to the global article index its file-local
// ordinal 0 corresponds to. The manifest orders articles file by file, so the
// base of file i is the sum of the article counts before it.
func FileRanges(files []jobFile) map[int32]int32 {
	base := int32(0)
	out := map[int32]int32{}
	for _, f := range files {
		out[f.FileIdx] = base
		base += int32(f.ArticleCount) //nolint:gosec // fixture article counts are tiny
	}
	return out
}

// ReadRegionCRC reads [off, off+length) of path and returns its CRC32, or
// ok=false when the file is shorter than the range.
func (h *harness) ReadRegionCRC(path string, off, length int64) (uint32, bool) {
	h.t.Helper()
	fh, err := os.Open(path) //nolint:gosec // harness-owned temp path
	if err != nil {
		h.t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = fh.Close() }()
	buf := make([]byte, length)
	n, err := fh.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		h.t.Fatalf("read %s at %d: %v", path, off, err)
	}
	if int64(n) < length {
		return 0, false
	}
	return crc32.ChecksumIEEE(buf), true
}

// ---------- helpers ----------

// deterministicPayload builds reproducible pseudo-random bytes for a file.
// Not compressible and not all-zero: a sparse-file bug that left a hole would
// read back as zeros and must not accidentally match the expected content.
func deterministicPayload(seed string, n int) []byte {
	out := make([]byte, n)
	state := uint64(1469598103934665603)
	for _, c := range []byte(seed) {
		state = (state ^ uint64(c)) * 1099511628211
	}
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = byte(state >> 24) //nolint:gosec // G115: a deliberate truncation to one pseudo-random byte
	}
	return out
}

// splitParts divides payload into chunks of at most partSize bytes.
func splitParts(payload []byte, partSize int) [][]byte {
	if partSize <= 0 || len(payload) <= partSize {
		return [][]byte{payload}
	}
	var parts [][]byte
	for off := 0; off < len(payload); off += partSize {
		parts = append(parts, payload[off:min(off+partSize, len(payload))])
	}
	return parts
}

var (
	reservedPortsMu sync.Mutex
	reservedPorts   = make(map[int]bool)
)

// freePort reserves and releases an ephemeral port, returning its number.
// Note: uniqueness tracking via reservedPorts is in-process only across
// concurrent tests; it cannot protect against external processes binding
// to the port between close and daemon startup.
func freePort(t *testing.T) int {
	t.Helper()
	reservedPortsMu.Lock()
	defer reservedPortsMu.Unlock()
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			_ = ln.Close()
			time.Sleep(5 * time.Millisecond)
			continue
		}
		port := addr.Port
		if err := ln.Close(); err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if !reservedPorts[port] {
			reservedPorts[port] = true
			return port
		}
	}
	t.Fatalf("failed to reserve an uncollided port after 50 attempts")
	return 0
}

// describeDiff summarises how two byte slices differ, in the terms someone
// reading a 3am failure needs: where the damage starts, how far it reaches, how
// much of it there is, and whether it looks like a hole.
//
// It deliberately does NOT report the length of the first contiguous differing
// run. That is what it used to do, and a 12 MiB hole printed as "2 bytes
// differ" whenever the expected payload happened to contain a zero two bytes
// in — the run ends at the first coincidental match, which says nothing about
// the size of the damage.
func describeDiff(got, want []byte, partSize int) string {
	n := min(len(got), len(want))
	first, last, count, zeros := -1, -1, 0, 0
	for i := range n {
		if got[i] == want[i] {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
		count++
		if got[i] == 0 {
			zeros++
		}
	}
	if first < 0 {
		return fmt.Sprintf("no differing byte in the common prefix of %d", n)
	}
	return fmt.Sprintf("offset %d (part %d, offset %d within it); %d bytes differ across "+
		"[%d,%d]; %d of them are zero on disk (%s)",
		first, first/partSize, first%partSize, count, first, last, zeros,
		holeVerdict(count, zeros))
}

// holeVerdict names the shape the differing bytes have, since "the region is
// zeros" and "the region holds the wrong data" are different defects.
func holeVerdict(count, zeros int) string {
	switch zeros {
	case count:
		return "every differing byte is zero: a hole, not wrong data"
	case 0:
		return "no differing byte is zero: wrong data, not a hole"
	default:
		return "a mix of zeros and wrong data"
	}
}

// str, i64 and f64 read a JSON value that the API may render as a string or
// a number, which the legacy mode-dispatch API does inconsistently by field.
func str(v any) string {
	s, _ := v.(string)
	return s
}

func i64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		var n int64
		_, _ = fmt.Sscanf(strings.ReplaceAll(t, ",", ""), "%d", &n)
		return n
	}
	return 0
}

func f64(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		var n float64
		_, _ = fmt.Sscanf(strings.ReplaceAll(t, ",", ""), "%f", &n)
		return n
	}
	return 0
}

// buildBinary compiles the daemon once for the package.
func buildBinary(dir string) (string, error) {
	out := filepath.Join(dir, "gonzbd")
	//nolint:gosec // G204: every argument is a constant or this package's own temp path
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", out, "./cmd/gonzbd")
	cmd.Dir = repoRoot()
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./cmd/gonzbd: %w\n%s", err, b)
	}
	return out, nil
}

// repoRoot returns the module root, two levels up from test/crash.
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}
