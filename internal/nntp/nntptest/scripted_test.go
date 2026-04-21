package nntptest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/nntp"
	"github.com/hobeone/sabnzbd-go/internal/nntp/nntptest"
)

func TestScripted_FetchHappyPath(t *testing.T) {
	s := nntptest.New(t)
	s.AddArticle("abc@host", []byte("line1\nline2\n"))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, s.ServerConfig("test", 1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	body, err := conn.Fetch(ctx, "abc@host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, want := string(body), "line1\nline2\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestScripted_DotStuffing(t *testing.T) {
	s := nntptest.New(t)
	s.AddArticle("dot@host", []byte(".leading dot\nnormal\n"))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, s.ServerConfig("test", 1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	body, err := conn.Fetch(ctx, "dot@host")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Client reverses the dot-stuffing, so the leading '.' survives as one char.
	if got, want := string(body), ".leading dot\nnormal\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestScripted_UnknownMessageIDReturns430(t *testing.T) {
	s := nntptest.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, s.ServerConfig("test", 1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_, err = conn.Fetch(ctx, "missing@host")
	if !errors.Is(err, nntp.ErrNoArticle) {
		t.Fatalf("Fetch err = %v, want ErrNoArticle", err)
	}
}

func TestScripted_FailureNotFoundIsOneShot(t *testing.T) {
	s := nntptest.New(t)
	s.AddArticle("retry@host", []byte("body\n"))
	s.InjectFailure("retry@host", nntptest.FailureNotFound)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, s.ServerConfig("test", 1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := conn.Fetch(ctx, "retry@host"); !errors.Is(err, nntp.ErrNoArticle) {
		t.Fatalf("first Fetch err = %v, want ErrNoArticle", err)
	}
	body, err := conn.Fetch(ctx, "retry@host")
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got, want := string(body), "body\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestScripted_DropMidBodyClosesConnection(t *testing.T) {
	s := nntptest.New(t)
	s.AddArticle("drop@host", []byte("abcdefghij\n"))
	s.InjectFailure("drop@host", nntptest.FailureDropMidBody)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := nntp.Dial(ctx, s.ServerConfig("test", 1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := conn.Fetch(ctx, "drop@host"); err == nil {
		t.Fatal("Fetch succeeded, expected error from dropped connection")
	}
}

func TestScripted_StallIsCancellable(t *testing.T) {
	s := nntptest.New(t)
	s.AddArticle("stall@host", []byte("body\n"))
	s.InjectFailure("stall@host", nntptest.FailureStall)

	dialCtx, dialCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer dialCancel()

	conn, err := nntp.Dial(dialCtx, s.ServerConfig("test", 1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	fetchCtx, fetchCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer fetchCancel()

	start := time.Now()
	_, err = conn.Fetch(fetchCtx, "stall@host")
	if err == nil {
		t.Fatal("Fetch succeeded, expected ctx cancellation")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Fetch did not respect context; took %v", elapsed)
	}
}

func TestScripted_MultipleConnectionsServedConcurrently(t *testing.T) {
	s := nntptest.New(t)
	s.AddArticle("one@host", []byte("1\n"))
	s.AddArticle("two@host", []byte("2\n"))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	cfg := s.ServerConfig("test", 2)
	conn1, err := nntp.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("Dial 1: %v", err)
	}
	t.Cleanup(func() { _ = conn1.Close() })

	conn2, err := nntp.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("Dial 2: %v", err)
	}
	t.Cleanup(func() { _ = conn2.Close() })

	b1, err := conn1.Fetch(ctx, "one@host")
	if err != nil {
		t.Fatalf("Fetch 1: %v", err)
	}
	b2, err := conn2.Fetch(ctx, "two@host")
	if err != nil {
		t.Fatalf("Fetch 2: %v", err)
	}
	if string(b1) != "1\n" || string(b2) != "2\n" {
		t.Errorf("bodies = %q, %q", b1, b2)
	}
}
