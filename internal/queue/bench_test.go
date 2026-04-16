package queue

import (
	"fmt"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/nzb"
)

// buildCorpus creates a Queue populated with numJobs jobs, each containing
// filesPerJob files with articlesPerFile articles. When mostlyDone is true,
// 95% of articles are pre-marked Done.
func buildCorpus(b *testing.B, numJobs, filesPerJob, articlesPerFile int, mostlyDone bool) *Queue {
	b.Helper()
	q := New()
	now := time.Now().UTC()

	for j := 0; j < numJobs; j++ {
		parsed := &nzb.NZB{}
		parsed.Files = make([]nzb.File, filesPerJob)
		for f := 0; f < filesPerJob; f++ {
			file := nzb.File{
				Subject: fmt.Sprintf("job%d-file%d.bin (1/1)", j, f),
				Date:    now,
				Groups:  []string{"alt.binaries.bench"},
			}
			file.Articles = make([]nzb.Article, articlesPerFile)
			for a := 0; a < articlesPerFile; a++ {
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
		})
		if err != nil {
			b.Fatalf("NewJob: %v", err)
		}

		if mostlyDone {
			// Mark 95% of articles as done before adding to queue.
			total := filesPerJob * articlesPerFile
			doneCount := (total * 95) / 100
			marked := 0
			for fi := range job.Files {
				for ai := range job.Files[fi].Articles {
					if marked >= doneCount {
						break
					}
					job.Files[fi].Articles[ai].Done = true
					marked++
				}
				if marked >= doneCount {
					break
				}
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

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
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

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
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

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.ForEachUnfinishedArticle(func(UnfinishedArticle) bool {
			return true
		})
	}
}
