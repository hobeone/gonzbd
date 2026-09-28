package downloader

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// uuBody is "Hello" UU-encoded: length char '%' (5 bytes), then one 4-char
// group plus the zero-length terminator. Shared with
// TestDecodePayload_UUYieldsACRCOverTheDecodedBytes in dmca_test.go.
const uuBody = "begin 644 test.bin\n%2&5L;&\\`\n`\nend\n"

// TestDecodePayload_UUMultipartRejected pins #346 — assertion E5 of
// docs/article-validation-contract.md. A UU decode only carries
// a discoverable offset for the first segment of a file. decodePayload must
// reject a UU decode that satisfies a request for any other segment, rather
// than asserting offset 0 for it — offset 0 belongs to segment 1, a different
// article.
func TestDecodePayload_UUMultipartRejected(t *testing.T) {
	t.Parallel()

	got, err := decodePayload([]byte(uuBody), 2)
	if !errors.Is(err, ErrUUMultipart) {
		t.Fatalf("decodePayload(part=2) err = %v, want ErrUUMultipart", err)
	}
	if got.data != nil {
		t.Errorf("decodePayload(part=2) data = %q, want nil — a rejected decode must not leak a buffer", got.data)
	}
}

// TestDecodePayload_UUSinglePartStillDecodes is the companion pin: segment 1
// is the one segment for which offset 0 is actually correct, and the E5
// guard must not reject it.
func TestDecodePayload_UUSinglePartStillDecodes(t *testing.T) {
	t.Parallel()

	got, err := decodePayload([]byte(uuBody), 1)
	if err != nil {
		t.Fatalf("decodePayload(part=1): %v", err)
	}
	if string(got.data) != "Hello" {
		t.Errorf("decodePayload(part=1) data = %q, want %q", got.data, "Hello")
	}
	if got.offset != 0 {
		t.Errorf("decodePayload(part=1) offset = %d, want 0", got.offset)
	}
}

// TestClassifyDecodeError_UUMultipart pins that the E5 rejection gets its
// own telemetry class rather than falling into the generic decode_failed
// bucket — the whole point of rejecting explicitly is that the diagnosis
// names UU as the cause, which #346 found missing.
func TestClassifyDecodeError_UUMultipart(t *testing.T) {
	t.Parallel()

	if got := classifyDecodeError(ErrUUMultipart); got != telemetry.ErrClassUUMultipart {
		t.Errorf("classifyDecodeError(ErrUUMultipart) = %q, want %q", got, telemetry.ErrClassUUMultipart)
	}
}

// TestProcessFetchedArticle_UUMultipartIsTerminal drives the full decode
// path processFetchedArticle uses in production: a UU body served for a
// requested segment > 1 must emit ErrUUMultipart and be counted under its
// own telemetry class, not swallowed into the generic decode_failed bucket
// (see TestClassifyDecodeError_UUMultipart for why that distinction is the
// point of this fix).
func TestProcessFetchedArticle_UUMultipartIsTerminal(t *testing.T) {
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	d := &Downloader{
		dispatcher:  newTestDispatcher(t),
		completions: make(chan *ArticleResult, 4),
		log:         slog.New(slog.DiscardHandler),
		tracker:     newDispatchTracker(),
	}
	srv := NewServer(config.ServerConfig{Name: "test-server"})
	req := &articleRequest{
		jobID: "job1", fileIdx: 0, messageID: "msg1", partNumber: 2,
	}

	d.processFetchedArticle(t.Context(), srv, req, []byte(uuBody))

	select {
	case res := <-d.completions:
		if !errors.Is(res.Err, ErrUUMultipart) {
			t.Errorf("Err = %v, want ErrUUMultipart", res.Err)
		}
	default:
		t.Fatal("expected a result to be emitted")
	}
	if got := telemetry.ErrorCount(telemetry.ErrClassUUMultipart); got != 1 {
		t.Errorf("PipelineErrors[uu_multipart] = %d, want 1", got)
	}
	if got := telemetry.ErrorCount(telemetry.ErrClassDecodeFailed); got != 0 {
		t.Errorf("PipelineErrors[decode_failed] = %d, want 0 — the rejection must not double-count into the generic bucket", got)
	}
}
