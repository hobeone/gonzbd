package assembler

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// A write whose owner is not the one that opened the handle under its key is
// sent through the resolver instead of the open handle; when the resolver
// refuses it, the article is dropped and the handle's owner is undisturbed.
func TestProcessRequest_RefusesAnotherOwnersWriteOnAnOpenHandle(t *testing.T) {
	dir := t.TempDir()
	a1, a2 := &testOwner{id: "job-1"}, &testOwner{id: "job-1"}
	var calls sync.Map // owner → call count
	var written atomic.Int32
	done := make(chan struct{})
	a := startAssembler(t, Options{
		FileInfo: func(o Owner, _ int) (FileInfo, error) {
			n, _ := calls.LoadOrStore(o, new(atomic.Int32))
			n.(*atomic.Int32).Add(1)
			if o == a2 {
				return FileInfo{}, fmt.Errorf("resolve: %w", ErrJobClosed)
			}
			return FileInfo{Dir: dir, Name: "f", TotalParts: 2}, nil
		},
		OnArticleWritten: func(string, int, int32, int64, int64, uint32) { written.Add(1) },
		OnFileComplete:   func(string, int) { close(done) },
	})
	write := func(o Owner, artIdx int32, off int64, data string) {
		t.Helper()
		if err := a.WriteArticle(t.Context(),
			ArticleRef{Owner: o, FileIdx: 0, ArtIdx: artIdx, MessageID: fmt.Sprintf("%d@x", artIdx)},
			WriteRequest{Offset: off, Data: []byte(data)}); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}
	write(a1, 0, 0, "AAAA") // opens the handle under a1
	write(a2, 0, 0, "BBBB") // a straggler of another instance for the same key
	write(a1, 1, 4, "CCCC") // completes a1's file
	<-done

	if got := string(readFile(t, filepath.Join(dir, "f"))); got != "AAAACCCC" {
		t.Errorf("file = %q, want a1's bytes only", got)
	}
	if n := written.Load(); n != 2 {
		t.Errorf("OnArticleWritten fired %d times, want 2", n)
	}
	n2, ok := calls.Load(a2)
	if !ok || n2.(*atomic.Int32).Load() != 1 {
		t.Errorf("the resolver was not asked once about a2's write; it must decide, not the open handle")
	}
}

// When the resolver accepts the other owner, the handle under the key belongs
// to an instance that has left (a CancelJob that timed out leaves handles
// open): it is closed with its file kept, and the request opens its own.
func TestProcessRequest_EvictsADepartedOwnersHandle(t *testing.T) {
	dir := t.TempDir()
	a1, a2 := &testOwner{id: "job-1"}, &testOwner{id: "job-1"}
	var resolvedA2 atomic.Int32
	done := make(chan struct{})
	a := startAssembler(t, Options{
		FileInfo: func(o Owner, _ int) (FileInfo, error) {
			if o == a2 {
				resolvedA2.Add(1)
				return FileInfo{Dir: dir, Name: "two", TotalParts: 1}, nil
			}
			return FileInfo{Dir: dir, Name: "one", TotalParts: 2}, nil
		},
		OnFileComplete: func(string, int) { close(done) },
	})
	if err := a.WriteArticle(t.Context(),
		ArticleRef{Owner: a1, FileIdx: 0, ArtIdx: 0, MessageID: "0@x"},
		WriteRequest{Offset: 0, Data: []byte("AAAA")}); err != nil {
		t.Fatalf("WriteArticle a1: %v", err)
	}
	if err := a.WriteArticle(t.Context(),
		ArticleRef{Owner: a2, FileIdx: 0, ArtIdx: 0, MessageID: "0@y"},
		WriteRequest{Offset: 0, Data: []byte("BBBB")}); err != nil {
		t.Fatalf("WriteArticle a2: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a2's file never completed: its write went to a1's handle")
	}

	if got := string(readFile(t, filepath.Join(dir, "two"))); got != "BBBB" {
		t.Errorf("a2's file = %q, want BBBB", got)
	}
	if got := string(readFile(t, filepath.Join(dir, "one"))); got != "AAAA" {
		t.Errorf("a1's file = %q, want its bytes kept, not deleted", got)
	}
	if n := resolvedA2.Load(); n == 0 {
		t.Error("a2's write never reached the resolver: it reused a1's handle")
	}
}
