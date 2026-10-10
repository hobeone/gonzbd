package jobtest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
)

func TestMarkArticleWritten_MarksTheArticleDoneInItsOwnFile(t *testing.T) {
	m := job.NewManifest([]job.JobFile{
		{Subject: "f0", Bytes: 200, Articles: []job.JobArticle{
			{ID: "<a0@x>", Bytes: 100, Number: 1},
			{ID: "<a1@x>", Bytes: 100, Number: 2},
		}},
		{Subject: "f1", Bytes: 100, Articles: []job.JobArticle{
			{ID: "<b0@x>", Bytes: 100, Number: 1},
		}},
	})
	j := job.New("j", "j", job.Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	MarkArticleWritten(t, j, 2)

	p := j.Progress()
	if !p.ArticleDone(2) {
		t.Error("article 2 is not Done")
	}
	if p.ArticleDone(0) || p.ArticleDone(1) {
		t.Error("an article of file 0 was marked Done; the helper must place the row in the article's own file")
	}
	if rows := j.FileRows(1); len(rows) != 1 || rows[0].ArtIdx != 2 {
		t.Errorf("file 1 rows = %+v, want one row for article 2", rows)
	}
}

func TestSeedFileCRC_SettlesTheFileToTheGivenCRC(t *testing.T) {
	m := job.NewManifest([]job.JobFile{
		{Subject: "f0", Bytes: 200, Articles: []job.JobArticle{
			{ID: "<a0@x>", Bytes: 100, Number: 1},
			{ID: "<a1@x>", Bytes: 100, Number: 2},
		}},
		{Subject: "f1", Bytes: 100, Articles: []job.JobArticle{
			{ID: "<b0@x>", Bytes: 100, Number: 1},
		}},
	})
	j := job.New("j", "j", job.Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}

	SeedFileCRC(t, j, 0, 0xCAFEF00D)

	p := j.Progress()
	if got := p.FileAssembledCRC32(0); got != 0xCAFEF00D {
		t.Errorf("file 0 CRC = %#x, want 0xcafef00d", got)
	}
	if !p.ArticleDone(0) || !p.ArticleDone(1) {
		t.Error("an article of the seeded file is not Done")
	}
	if p.ArticleDone(2) {
		t.Error("an article of another file was marked Done")
	}
}

// fatalTB turns Fatalf into a panic carrying the message, so a test can
// observe that a helper refused its input.
type fatalTB struct {
	testing.TB
}

type fatalMsg string

func (fatalTB) Helper() {}

func (fatalTB) Fatalf(format string, args ...any) {
	panic(fatalMsg(fmt.Sprintf(format, args...)))
}

func fatalOf(t *testing.T, fn func(testing.TB)) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		m, ok := r.(fatalMsg)
		if !ok {
			t.Fatalf("helper did not call Fatalf (recovered %v)", r)
		}
		msg = string(m)
	}()
	fn(fatalTB{t})
	return ""
}

func TestHelpers_RefuseAJobTheyCannotPlace(t *testing.T) {
	bare := job.New("bare", "bare", job.Policy{})
	if msg := fatalOf(t, func(tb testing.TB) { MarkArticleWritten(tb, bare, 0) }); !strings.Contains(msg, "manifest of bare") {
		t.Errorf("MarkArticleWritten on a job with no manifest: %q", msg)
	}
	if msg := fatalOf(t, func(tb testing.TB) { SeedFileCRC(tb, bare, 0, 1) }); !strings.Contains(msg, "manifest of bare") {
		t.Errorf("SeedFileCRC on a job with no manifest: %q", msg)
	}

	m := job.NewManifest([]job.JobFile{{Subject: "f", Bytes: 10, Articles: []job.JobArticle{{ID: "<a@x>", Bytes: 10, Number: 1}}}})
	j := job.New("j", "j", job.Policy{})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	if msg := fatalOf(t, func(tb testing.TB) { MarkArticleWritten(tb, j, 5) }); !strings.Contains(msg, "out of range") {
		t.Errorf("MarkArticleWritten out of range: %q", msg)
	}
}
