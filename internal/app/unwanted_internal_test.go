package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// TestUnwantedFailMessage pins the message's shape: SABnzbd's prefix, then
// at most three names, then a count of the rest.
func TestUnwantedFailMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{"a.exe"}, "Aborted, unwanted extension detected: a.exe"},
		{[]string{"a.exe", "b.scr", "c.bat"}, "Aborted, unwanted extension detected: a.exe, b.scr, c.bat"},
		{[]string{"a.exe", "b.scr", "c.bat", "d.cmd", "e.msi"}, "Aborted, unwanted extension detected: a.exe, b.scr, c.bat and 2 more"},
	}
	for _, c := range cases {
		if got := unwantedFailMessage(c.names); got != c.want {
			t.Errorf("unwantedFailMessage(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}

// TestScreenUnwanted drives the check directly, one branch per case: what
// it records on the header, whether it pauses the job, and what it returns.
func TestScreenUnwanted(t *testing.T) {
	t.Parallel()
	const (
		clean = "movie.mkv"
		bad   = "setup.exe"
	)
	cases := []struct {
		name      string
		action    unwanted.Action
		mode      unwanted.Mode
		files     []string
		approved  bool
		prior     unwanted.State
		wantState unwanted.State
		wantPause bool
		wantFail  bool
		wantErr   bool
	}{
		{name: "pause blocks", action: unwanted.ActionPause, files: []string{clean, bad}, wantState: unwanted.StateBlocked, wantPause: true},
		{name: "fail blocks and fails", action: unwanted.ActionFail, files: []string{clean, bad}, wantState: unwanted.StateBlocked, wantPause: true, wantFail: true},
		{name: "off checks nothing", action: unwanted.ActionOff, files: []string{bad}, wantState: unwanted.StateNone},
		{name: "a clean job is not blocked", action: unwanted.ActionFail, files: []string{clean}, prior: unwanted.StateApproved, wantState: unwanted.StateNone},
		{name: "an approved job is not checked", action: unwanted.ActionFail, files: []string{bad}, approved: true, wantState: unwanted.StateApproved},
		{name: "unreadable rules fail closed", action: unwanted.ActionPause, mode: "graylist", files: []string{bad}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := newTestApplication(t)
			mode := c.mode
			if mode == "" {
				mode = unwanted.ModeBlacklist
			}
			a.config.With(func(cfg *config.Config) {
				cfg.Downloads.ActionOnUnwantedExtensions = c.action
				cfg.Downloads.UnwantedExtensionsMode = mode
				cfg.Downloads.UnwantedExtensions = []string{"exe"}
			})
			// nzb.Parse stores the filename it extracts from the subject
			// line in File.Subject, so a parsed file's Subject is its name.
			parsed := &nzb.NZB{}
			for i, name := range c.files {
				parsed.Files = append(parsed.Files, nzb.File{
					Subject:  name,
					Bytes:    100,
					Articles: []nzb.Article{{ID: fmt.Sprintf("a%d@t", i), Bytes: 100, Number: 1}},
				})
			}
			j, hdr, err := BuildIngestJob(a.config, parsed, "t.nzb", types.FetchOptions{NzbName: "t.nzb"}, nil)
			if err != nil {
				t.Fatalf("BuildIngestJob: %v", err)
			}
			hdr.Unwanted = c.prior

			failMsg, err := a.screenUnwanted(j, &hdr, c.approved)
			if c.wantErr {
				if err == nil {
					t.Fatalf("screenUnwanted = nil error; want the check to fail closed")
				}
				return
			}
			if err != nil {
				t.Fatalf("screenUnwanted: %v", err)
			}
			if hdr.Unwanted != c.wantState {
				t.Errorf("hdr.Unwanted = %d, want %d", hdr.Unwanted, c.wantState)
			}
			if paused := j.Intent() == job.IntentPause; paused != c.wantPause {
				t.Errorf("paused = %v, want %v", paused, c.wantPause)
			}
			if c.wantFail != (failMsg != "") {
				t.Errorf("failMsg = %q, want a message: %v", failMsg, c.wantFail)
			}
			if c.wantFail && !strings.Contains(failMsg, bad) {
				t.Errorf("failMsg = %q, want it to name %s", failMsg, bad)
			}
		})
	}
}
