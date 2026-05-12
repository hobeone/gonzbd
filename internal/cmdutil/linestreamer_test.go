package cmdutil

import (
	"strings"
	"sync"
	"testing"
)

func TestLineStreamer_SingleLine(t *testing.T) {
	var got []string
	s := NewLineStreamer(func(line string) { got = append(got, line) })
	s.Write([]byte("hello world\n"))
	if len(got) != 1 || got[0] != "hello world" {
		t.Errorf("got %v, want [hello world]", got)
	}
}

func TestLineStreamer_MultiLine(t *testing.T) {
	var got []string
	s := NewLineStreamer(func(line string) { got = append(got, line) })
	s.Write([]byte("alpha\nbeta\ngamma\n"))
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLineStreamer_PartialLine(t *testing.T) {
	var got []string
	s := NewLineStreamer(func(line string) { got = append(got, line) })
	s.Write([]byte("hel"))
	if len(got) != 0 {
		t.Fatalf("expected no lines yet, got %v", got)
	}
	s.Write([]byte("lo\n"))
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("got %v, want [hello]", got)
	}
}

func TestLineStreamer_Flush(t *testing.T) {
	var got []string
	s := NewLineStreamer(func(line string) { got = append(got, line) })
	s.Write([]byte("partial"))
	if len(got) != 0 {
		t.Fatalf("expected no lines before flush, got %v", got)
	}
	s.Flush()
	if len(got) != 1 || got[0] != "partial" {
		t.Errorf("got %v, want [partial]", got)
	}
}

func TestLineStreamer_FlushEmpty(t *testing.T) {
	var got []string
	s := NewLineStreamer(func(line string) { got = append(got, line) })
	s.Flush() // no-op on empty
	if len(got) != 0 {
		t.Errorf("expected no lines, got %v", got)
	}
}

func TestLineStreamer_CRLF(t *testing.T) {
	var got []string
	s := NewLineStreamer(func(line string) { got = append(got, line) })
	s.Write([]byte("line1\r\nline2\r\n"))
	want := []string{"line1", "line2"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLineStreamer_String(t *testing.T) {
	s := NewLineStreamer(nil)
	s.Write([]byte("first\nsecond\n"))
	if got := s.String(); got != "first\nsecond\n" {
		t.Errorf("String() = %q, want %q", got, "first\nsecond\n")
	}
}

func TestLineStreamer_NilOnLine(t *testing.T) {
	s := NewLineStreamer(nil)
	// Should not panic with nil callback.
	s.Write([]byte("no callback\n"))
	s.Flush()
	if got := s.String(); got != "no callback\n" {
		t.Errorf("String() = %q", got)
	}
}

func TestLineStreamer_Capped(t *testing.T) {
	var lines []string
	s := NewLineStreamerCapped(func(line string) { lines = append(lines, line) }, 10)
	// Write more than 10 bytes total.
	s.Write([]byte("12345\n"))   // 6 bytes → full has 6
	s.Write([]byte("6789abc\n")) // 8 bytes → full gets 4 more then caps
	s.Flush()

	// Lines callback should still fire for complete lines.
	if len(lines) != 2 {
		t.Errorf("got %d lines, want 2: %v", len(lines), lines)
	}

	// String() should be truncated at cap.
	if got := s.String(); len(got) > 10 {
		t.Errorf("String() len=%d, want <=10", len(got))
	}
}

func TestLineStreamer_ConcurrentWrites(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	s := NewLineStreamer(func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.Write([]byte("line\n"))
			_ = n
		}(i)
	}
	wg.Wait()
	s.Flush()

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 100 {
		t.Errorf("got %d lines, want 100", len(lines))
	}
}

func TestLineStreamer_MixedWriteSizes(t *testing.T) {
	var got []string
	s := NewLineStreamer(func(line string) { got = append(got, line) })
	// Simulate byte-at-a-time writing (like a slow subprocess).
	msg := "slow output here\n"
	for _, b := range []byte(msg) {
		s.Write([]byte{b})
	}
	if len(got) != 1 || got[0] != "slow output here" {
		t.Errorf("got %v, want [slow output here]", got)
	}
}

func TestLineStreamer_EmptyLines(t *testing.T) {
	var got []string
	s := NewLineStreamer(func(line string) { got = append(got, line) })
	s.Write([]byte("\n\nfoo\n\n"))
	want := []string{"", "", "foo", ""}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLineStreamer_Bytes(t *testing.T) {
	s := NewLineStreamer(nil)
	s.Write([]byte("abc"))
	if got := string(s.Bytes()); got != "abc" {
		t.Errorf("Bytes() = %q, want %q", got, "abc")
	}
}

func TestLineStreamer_LargeOutput(t *testing.T) {
	// Verify no issues with large single writes.
	var count int
	s := NewLineStreamer(func(_ string) { count++ })
	// 10000 lines in one write.
	var b strings.Builder
	for range 10000 {
		b.WriteString("line\n")
	}
	s.Write([]byte(b.String()))
	if count != 10000 {
		t.Errorf("got %d lines, want 10000", count)
	}
}
