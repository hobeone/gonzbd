package assembler

import (
	"context"
	"errors"
	"testing"
)

// A ref without an owner is refused at the door: production builds every
// ref from an ArticleResult, which carries its instance, so a nil owner is a
// caller that bypassed that, and the worker must not be left to infer one.
func TestWriteArticle_RefusesARefWithoutAnOwner(t *testing.T) {
	a := startAssembler(t, Options{
		FileInfo: func(Owner, int) (FileInfo, error) {
			t.Error("the resolver was reached for a ref with no owner")
			return FileInfo{}, nil
		},
	})
	err := a.WriteArticle(context.Background(),
		ArticleRef{FileIdx: 0, ArtIdx: 0, MessageID: "m@x"},
		WriteRequest{Data: []byte("x")})
	if !errors.Is(err, ErrNoOwner) {
		t.Fatalf("WriteArticle with a nil owner = %v, want ErrNoOwner", err)
	}
}

// The resolver is handed the owner the ref carried, and the ID the worker
// keys and reports by is derived from it.
func TestOpenTargetFile_ResolvesWithTheRefsOwner(t *testing.T) {
	dir := t.TempDir()
	owner := &testOwner{id: "job-1"}
	var got Owner
	var gotJobID string
	done := make(chan struct{})
	a := startAssembler(t, Options{
		FileInfo: func(o Owner, _ int) (FileInfo, error) {
			got = o
			return FileInfo{Dir: dir, Name: "f", TotalParts: 1}, nil
		},
		OnArticleWritten: func(jobID string, _ int, _ int32, _, _ int64, _ uint32) {
			gotJobID = jobID
		},
		OnFileComplete: func(string, int) { close(done) },
	})
	if err := a.WriteArticle(t.Context(),
		ArticleRef{Owner: owner, FileIdx: 0, ArtIdx: 0, MessageID: "m@x"},
		WriteRequest{Offset: 0, Data: []byte("abc")}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	<-done
	if got != owner {
		t.Fatalf("the resolver received %v, want the ref's owner", got)
	}
	if gotJobID != "job-1" {
		t.Fatalf("OnArticleWritten jobID = %q, want the owner's ID", gotJobID)
	}
}
