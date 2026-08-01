package queue

import (
	"fmt"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// buildCorpus creates a Queue populated with numJobs jobs, each containing
// filesPerJob files with articlesPerFile articles. When mostlyDone is true,
// 95% of articles are pre-marked Done.
func buildCorpus(b *testing.B, numJobs, filesPerJob, articlesPerFile int, mostlyDone bool) *Queue {
	b.Helper()
	q := New()
	now := time.Now().UTC()

	for j := range numJobs {
		parsed := &nzb.NZB{}
		parsed.Files = make([]nzb.File, filesPerJob)
		for f := range filesPerJob {
			file := nzb.File{
				Subject: fmt.Sprintf("job%d-file%d.bin (1/1)", j, f),
				Date:    now,
				Groups:  []string{"alt.binaries.bench"},
			}
			file.Articles = make([]nzb.Article, articlesPerFile)
			for a := range articlesPerFile {
				file.Articles[a] = nzb.Article{
					ID:     fmt.Sprintf("art%d-%d-%d@bench.test", j, f, a),
					Bytes:  65536,
					Number: a + 1,
				}
				file.Bytes += 65536
			}
			parsed.Files[f] = file
		}

		job, err := NewJob(parsed, AddOptions{
			Filename: fmt.Sprintf("bench%d.nzb", j),
		}, fsutil.SanitizeOptions{})
		if err != nil {
			b.Fatalf("NewJob: %v", err)
		}

		if mostlyDone {
			// Mark 95% of articles as done before adding to queue.
			total := filesPerJob * articlesPerFile
			doneCount := (total * 95) / 100
			for i := 0; i < doneCount && i < job.manifest.NumArticles(); i++ {
				job.progress.done.Set(i)
			}
		}

		if err := q.Add(job); err != nil {
			b.Fatalf("Add: %v", err)
		}
	}
	return q
}

// BenchmarkForEachUnfinishedArticle_1000x100 benchmarks a full iteration over
// 1000 jobs with ~100 unfinished articles each (10 files × 10 articles per file).
func BenchmarkForEachUnfinishedArticle_1000x100(b *testing.B) {
	const (
		numJobs         = 1000
		filesPerJob     = 10
		articlesPerFile = 10
	)
	q := buildCorpus(b, numJobs, filesPerJob, articlesPerFile, false)
	expectedTotal := numJobs * filesPerJob * articlesPerFile

	for b.Loop() {
		count := 0
		q.ForEachUnfinishedArticle(func(UnfinishedArticle) bool {
			count++
			return true
		})
		if count != expectedTotal {
			b.Fatalf("unexpected article count: got %d, want %d", count, expectedTotal)
		}
	}
}

// BenchmarkForEachUnfinishedArticle_EarlyExit benchmarks the dispatcher's
// happy path where it stops after filling its work quota (10 articles) even
// though there are 100,000 articles in the queue.
func BenchmarkForEachUnfinishedArticle_EarlyExit(b *testing.B) {
	const (
		numJobs         = 1000
		filesPerJob     = 10
		articlesPerFile = 10
		quota           = 10
	)
	q := buildCorpus(b, numJobs, filesPerJob, articlesPerFile, false)

	for b.Loop() {
		count := 0
		q.ForEachUnfinishedArticle(func(UnfinishedArticle) bool {
			count++
			return count < quota
		})
	}
}

// BenchmarkForEachUnfinishedArticle_MostlyComplete benchmarks skip overhead:
// 95% of articles are already Done, so the iterator must scan many completed
// articles to find the sparse unfinished ones.
func BenchmarkForEachUnfinishedArticle_MostlyComplete(b *testing.B) {
	const (
		numJobs         = 1000
		filesPerJob     = 10
		articlesPerFile = 10
	)
	q := buildCorpus(b, numJobs, filesPerJob, articlesPerFile, true)

	for b.Loop() {
		q.ForEachUnfinishedArticle(func(UnfinishedArticle) bool {
			return true
		})
	}
}

// BenchmarkAdd_PendingFootprint reports bytes/op and allocs/op for adding
// header-heavy jobs that are never dispatched. The win from not building
// artIdx eagerly shows up as reduced allocs/op versus the pre-change
// baseline. This is a REPORTING benchmark, not a gate.
func BenchmarkAdd_PendingFootprint(b *testing.B) {
	parsed := &nzb.NZB{}
	parsed.Files = make([]nzb.File, 10)
	for f := range parsed.Files {
		arts := make([]nzb.Article, 20)
		for a := range arts {
			arts[a] = nzb.Article{ID: fmt.Sprintf("art%d-%d@bench.example", f, a), Bytes: 65536, Number: a + 1}
		}
		parsed.Files[f] = nzb.File{Subject: fmt.Sprintf("f%d.bin (1/1)", f), Date: time.Now().UTC(), Articles: arts}
	}
	b.ReportAllocs()
	for b.Loop() {
		job, _ := NewJob(parsed, AddOptions{Filename: "b.nzb"}, fsutil.SanitizeOptions{})
		q := New()
		_ = q.Add(job)
	}
}
