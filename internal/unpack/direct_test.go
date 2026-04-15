package unpack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedMock drives a pair of io.Pipes to simulate unrar's stdout/stdin
// protocol. Each step in the script is one of:
//   - write(s): emit s to stdout, blocking until the reader consumes it
//   - expect(s): read exactly len(s) bytes from stdin and verify they match
//
// The mock runs in its own goroutine. Errors (unexpected stdin content,
// pipe closure mid-step) are captured and surfaced via err after shutdown.
type scriptedMock struct {
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter

	script []mockStep

	mu   sync.Mutex
	err  error
	done chan struct{}
}

type mockStep struct {
	kind    stepKind
	payload string
}

type stepKind int

const (
	stepWrite stepKind = iota
	stepExpect
)

func newScriptedMock(script []mockStep) *scriptedMock {
	stdoutR, stdoutW := io.Pipe()
	stdinR, stdinW := io.Pipe()
	m := &scriptedMock{
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		stdinR:  stdinR,
		stdinW:  stdinW,
		script:  script,
		done:    make(chan struct{}),
	}
	go m.run()
	return m
}

func (m *scriptedMock) run() {
	defer close(m.done)
	defer func() { _ = m.stdoutW.Close() }() //nolint:errcheck // closing on shutdown; error is already captured in m.err
	for i, step := range m.script {
		switch step.kind {
		case stepWrite:
			if _, err := m.stdoutW.Write([]byte(step.payload)); err != nil {
				m.setErr(fmt.Errorf("step %d write: %w", i, err))
				return
			}
		case stepExpect:
			want := []byte(step.payload)
			buf := make([]byte, len(want))
			if _, err := io.ReadFull(m.stdinR, buf); err != nil {
				m.setErr(fmt.Errorf("step %d expect %q: %w", i, step.payload, err))
				return
			}
			if string(buf) != step.payload {
				m.setErr(fmt.Errorf("step %d: got stdin %q, want %q", i, string(buf), step.payload))
				return
			}
		}
	}
}

func (m *scriptedMock) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err == nil {
		m.err = err
	}
}

func (m *scriptedMock) shutdown() error {
	_ = m.stdoutW.Close() //nolint:errcheck // shutdown path; may already be closed by run()
	_ = m.stdinW.Close()  //nolint:errcheck // shutdown path
	select {
	case <-m.done:
	case <-time.After(2 * time.Second):
		return errors.New("mock goroutine did not shut down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func write(s string) mockStep  { return mockStep{kind: stepWrite, payload: s} }
func expect(s string) mockStep { return mockStep{kind: stepExpect, payload: s} }

func TestDirectUnpackLoop_HappyPath(t *testing.T) {
	t.Parallel()

	mock := newScriptedMock([]mockStep{
		write("\n\nUNRAR 6.02 freeware\n"),
		write("Extracting from /d/movie.part01.rar\n\n"),
		write("Extracting  movie.mkv     OK\n"),
		write("[C]ontinue, [Q]uit "),
		expect("C\n"),
		write("\n\nExtracting from /d/movie.part02.rar\n"),
		write("[C]ontinue, [Q]uit "),
		expect("C\n"),
		write("\n\nExtracting from /d/movie.part03.rar\n"),
		write("All OK\n"),
	})

	var askedFor []int
	wait := func(_ context.Context, idx int) (string, error) {
		askedFor = append(askedFor, idx)
		return fmt.Sprintf("/d/movie.part%02d.rar", idx), nil
	}

	result, err := directUnpackLoop(t.Context(), mock.stdoutR, mock.stdinW, 3, wait)
	if err != nil {
		t.Fatalf("directUnpackLoop: %v", err)
	}
	if mockErr := mock.shutdown(); mockErr != nil {
		t.Fatalf("mock: %v", mockErr)
	}

	if !result.Success {
		t.Errorf("Success = false; want true; output:\n%s", result.Output)
	}
	if want := []int{2, 3}; !intsEqual(askedFor, want) {
		t.Errorf("wait called with %v; want %v", askedFor, want)
	}
	if want := []string{"movie.mkv"}; !stringsEqual(result.ExtractedFiles, want) {
		t.Errorf("ExtractedFiles = %v; want %v", result.ExtractedFiles, want)
	}
}

func TestDirectUnpackLoop_FatalErrorAborts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
	}{
		{"crc_failed", "movie.mkv  - CRC failed in the encrypted file\n"},
		{"incorrect_password", "Incorrect password for movie.mkv\n"},
		{"cannot_create", "Cannot create file: movie.mkv\n"},
		{"generic_error", "ERROR: something went wrong\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock := newScriptedMock([]mockStep{
				write("\nExtracting from /d/movie.part01.rar\n"),
				write(tc.line),
				expect("Q\n"),
			})

			wait := func(_ context.Context, _ int) (string, error) {
				return "", errors.New("wait should not be called")
			}

			result, err := directUnpackLoop(t.Context(), mock.stdoutR, mock.stdinW, 3, wait)
			if err == nil {
				t.Fatalf("expected error; got nil (output: %s)", result.Output)
			}
			if !strings.Contains(err.Error(), "unrar fatal") {
				t.Errorf("err = %v; want contains 'unrar fatal'", err)
			}
			if result.Success {
				t.Errorf("Success = true; want false")
			}
			if mockErr := mock.shutdown(); mockErr != nil {
				t.Fatalf("mock: %v", mockErr)
			}
		})
	}
}

func TestDirectUnpackLoop_WaitErrorAborts(t *testing.T) {
	t.Parallel()

	mock := newScriptedMock([]mockStep{
		write("Extracting from /d/movie.part01.rar\n"),
		write("[C]ontinue, [Q]uit "),
		expect("Q\n"),
	})

	waitErr := errors.New("download cancelled")
	wait := func(_ context.Context, _ int) (string, error) {
		return "", waitErr
	}

	result, err := directUnpackLoop(t.Context(), mock.stdoutR, mock.stdinW, 3, wait)
	if err == nil {
		t.Fatalf("expected error; got nil")
	}
	if !errors.Is(err, waitErr) {
		t.Errorf("err = %v; want wrapping %v", err, waitErr)
	}
	if result.Success {
		t.Errorf("Success = true; want false")
	}
	if mockErr := mock.shutdown(); mockErr != nil {
		t.Fatalf("mock: %v", mockErr)
	}
}

func TestDirectUnpackLoop_RetryPromptAborts(t *testing.T) {
	t.Parallel()

	mock := newScriptedMock([]mockStep{
		write("Volume movie.part02.rar not found\n"),
		write("[R]etry, [A]bort "),
		expect("A\n"),
	})

	wait := func(_ context.Context, _ int) (string, error) {
		return "", errors.New("wait should not be called on retry prompt")
	}

	_, err := directUnpackLoop(t.Context(), mock.stdoutR, mock.stdinW, 3, wait)
	if err == nil {
		t.Fatalf("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "retry/abort") {
		t.Errorf("err = %v; want contains 'retry/abort'", err)
	}
	if mockErr := mock.shutdown(); mockErr != nil {
		t.Fatalf("mock: %v", mockErr)
	}
}

func TestDirectUnpackLoop_ExtraPromptBeyondTotal(t *testing.T) {
	t.Parallel()

	// TotalVolumes = 2 but unrar prompts three times. The third prompt
	// (asking for volume 4) must be answered with Q without calling wait.
	mock := newScriptedMock([]mockStep{
		write("Extracting from /d/movie.part01.rar\n"),
		write("[C]ontinue, [Q]uit "),
		expect("C\n"),
		write("\n[C]ontinue, [Q]uit "),
		expect("Q\n"),
		write("\nAll OK\n"),
	})

	calls := 0
	wait := func(_ context.Context, idx int) (string, error) {
		calls++
		if idx > 2 {
			t.Errorf("wait called for idx %d; total is 2", idx)
		}
		return fmt.Sprintf("/d/movie.part%02d.rar", idx), nil
	}

	result, err := directUnpackLoop(t.Context(), mock.stdoutR, mock.stdinW, 2, wait)
	if err != nil {
		t.Fatalf("directUnpackLoop: %v", err)
	}
	if calls != 1 {
		t.Errorf("wait called %d times; want 1 (volume 2 only)", calls)
	}
	if !result.Success {
		t.Errorf("Success = false; want true")
	}
	if mockErr := mock.shutdown(); mockErr != nil {
		t.Fatalf("mock: %v", mockErr)
	}
}

func TestDirectUnpackLoop_CapturesExtractedFiles(t *testing.T) {
	t.Parallel()

	mock := newScriptedMock([]mockStep{
		write("Extracting from /d/movie.part01.rar\n"),
		write("Extracting  movie.mkv      OK\n"),
		write("Extracting  sample/a.mkv    OK\n"),
		write("All OK\n"),
	})

	wait := func(_ context.Context, _ int) (string, error) {
		return "", errors.New("should not be called")
	}

	result, err := directUnpackLoop(t.Context(), mock.stdoutR, mock.stdinW, 1, wait)
	if err != nil {
		t.Fatalf("directUnpackLoop: %v", err)
	}
	want := []string{"movie.mkv", "sample/a.mkv"}
	if !stringsEqual(result.ExtractedFiles, want) {
		t.Errorf("ExtractedFiles = %v; want %v", result.ExtractedFiles, want)
	}
	if mockErr := mock.shutdown(); mockErr != nil {
		t.Fatalf("mock: %v", mockErr)
	}
}

func TestDirectUnpack_InputValidation(t *testing.T) {
	t.Parallel()

	wait := func(_ context.Context, _ int) (string, error) { return "", nil }

	cases := []struct {
		name    string
		input   DirectUnpackInput
		wait    VolumeWaiter
		wantErr string
	}{
		{
			name:    "nil_wait",
			input:   DirectUnpackInput{FirstVolumePath: "/x", OutDir: "/y", TotalVolumes: 1},
			wait:    nil,
			wantErr: "wait is nil",
		},
		{
			name:    "empty_first_volume",
			input:   DirectUnpackInput{OutDir: "/y", TotalVolumes: 1},
			wait:    wait,
			wantErr: "FirstVolumePath",
		},
		{
			name:    "empty_out_dir",
			input:   DirectUnpackInput{FirstVolumePath: "/x", TotalVolumes: 1},
			wait:    wait,
			wantErr: "OutDir",
		},
		{
			name:    "zero_total_volumes",
			input:   DirectUnpackInput{FirstVolumePath: "/x", OutDir: "/y"},
			wait:    wait,
			wantErr: "TotalVolumes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := DirectUnpack(t.Context(), tc.input, tc.wait)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v; want contains %q", err, tc.wantErr)
			}
		})
	}
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
