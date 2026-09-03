package app

import (
	"fmt"
	"testing"

	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestPipeline_MultiFile_ArtIdx_EndToEnd tests multi-file article completion
// through the full pipeline using ArtIdx to verify zero index drift or off-by-one errors.
func TestPipeline_MultiFile_ArtIdx_EndToEnd(t *testing.T) {
	// Create a 3-file NZB job with different article counts per file
	// File 0: 3 articles (indices 0, 1, 2)
	// File 1: 2 articles (indices 3, 4)
	// File 2: 4 articles (indices 5, 6, 7, 8)
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {"multifile-test"}},
		Groups: []string{"alt.binaries.test"},
		Files: []nzb.File{
			{
				Subject: "file0.rar",
				Bytes:   3000,
				Articles: []nzb.Article{
					{ID: "art0@test", Bytes: 1000, Number: 1},
					{ID: "art1@test", Bytes: 1000, Number: 2},
					{ID: "art2@test", Bytes: 1000, Number: 3},
				},
			},
			{
				Subject: "file1.rar",
				Bytes:   2000,
				Articles: []nzb.Article{
					{ID: "art3@test", Bytes: 1000, Number: 1},
					{ID: "art4@test", Bytes: 1000, Number: 2},
				},
			},
			{
				Subject: "file2.rar",
				Bytes:   4000,
				Articles: []nzb.Article{
					{ID: "art5@test", Bytes: 1000, Number: 1},
					{ID: "art6@test", Bytes: 1000, Number: 2},
					{ID: "art7@test", Bytes: 1000, Number: 3},
					{ID: "art8@test", Bytes: 1000, Number: 4},
				},
			},
		},
	}

	app := newTestApplication(t)
	disp := app.Dispatcher()
	disp.Pause()

	j, hdr, err := BuildIngestJob(app.config, parsed, "multifile-test.nzb", types.FetchOptions{NzbName: "multifile-test"}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob failed: %v", err)
	}
	if err := disp.Add(j, hdr); err != nil {
		t.Fatalf("disp.Add failed: %v", err)
	}

	m := mustManifest(t, j)
	if m.NumArticles() != 9 {
		t.Fatalf("Expected 9 total articles, got %d", m.NumArticles())
	}

	// Verify ForEachUnfinishedArticle yields correct ArtIdx and MessageID pairs
	type unfinishedItem struct {
		artIdx    int32
		messageID string
	}
	var yielded []unfinishedItem
	_ = j.ForEachUnfinishedArticle(func(fileIdx int, artIdx int32, id string, bytes int, number int, subject string) bool {
		yielded = append(yielded, unfinishedItem{artIdx: artIdx, messageID: id})
		return true
	})

	if len(yielded) != 9 {
		t.Fatalf("Expected 9 yielded articles, got %d", len(yielded))
	}

	for i, ua := range yielded {
		expectedID := fmt.Sprintf("art%d@test", i)
		if int(ua.artIdx) != i {
			t.Errorf("Article %d yielded wrong ArtIdx: got %d, want %d", i, ua.artIdx, i)
		}
		if ua.messageID != expectedID {
			t.Errorf("Article %d yielded wrong MessageID: got %s, want %s", i, ua.messageID, expectedID)
		}
	}

	// Mark article 3 (first article of File 1) done by ArtIdx
	ackDoneIdx(t, disp, j.ID(), 3)

	p := j.Progress()
	if p.FilePending(0) != 3 {
		t.Errorf("File 0 pending should be 3, got %d", p.FilePending(0))
	}
	if p.FilePending(1) != 1 {
		t.Errorf("File 1 pending should be 1 (since 1 of 2 done), got %d", p.FilePending(1))
	}
	if p.FilePending(2) != 4 {
		t.Errorf("File 2 pending should be 4, got %d", p.FilePending(2))
	}

	// Mark all articles done by ArtIdx
	allIdxs := make([]int32, 0, 9)
	for i := range 9 {
		allIdxs = append(allIdxs, int32(i))
	}
	ackDoneIdx(t, disp, j.ID(), allIdxs...)

	p = j.Progress()
	if p.PendingArticles() != 0 {
		t.Errorf("Expected 0 pending articles after marking all done, got %d", p.PendingArticles())
	}
	if p.ArticlesResolved() != 9 {
		t.Errorf("Expected 9 resolved articles, got %d", p.ArticlesResolved())
	}
}
