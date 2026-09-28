package downloader

import (
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// uuBody is "Hello" UU-encoded: length char '%' (5 bytes), then one 4-char
// group plus the zero-length terminator. Shared with
// TestDecodePayload_UUYieldsACRCOverTheDecodedBytes in dmca_test.go.
const uuBody = "begin 644 test.bin\n%2&5L;&\\`\n`\nend\n"

// yencNoYPart builds a yEnc article with no =ypart line, so
// decoder.Article.HasOffset is false and Offset defaults to 0. part is the
// =ybegin part= value; 0 omits the key entirely (a plain, single-part-shaped
// header). Mirrors yencPart in partnumber_test.go, which always includes
// =ypart and so cannot build this fixture.
func yencNoYPart(part int, payload string) []byte {
	encoded := make([]byte, 0, len(payload))
	for i := range len(payload) {
		encoded = append(encoded, payload[i]+42)
	}
	sum := crc32.ChecksumIEEE([]byte(payload))
	if part == 0 {
		return fmt.Appendf(nil,
			"=ybegin line=128 size=%d name=t.bin\r\n%s\r\n=yend size=%d crc32=%08x\r\n",
			len(payload), encoded, len(payload), sum)
	}
	return fmt.Appendf(nil,
		"=ybegin part=%d line=128 size=%d name=t.bin\r\n%s\r\n=yend size=%d part=%d pcrc32=%08x\r\n",
		part, len(payload), encoded, len(payload), part, sum)
}

// TestDecodePayload_UUPartGreaterThanOneRejected pins assertion E5 of
// docs/article-validation-contract.md: a UU decode only carries a
// discoverable offset for the first segment of a file (offset 0).
// decodePayload must reject a UU decode that satisfies a request for any
// other segment, rather than asserting offset 0 for it — offset 0 belongs to
// segment 1, a different article.
//
// A real multi-part UU continuation carries no "begin" line at all and is
// rejected earlier, by DecodeUU itself (ErrNotUU, uu.go's parts[0] != "begin"
// check) — that path is unaffected by this guard and still classifies as
// decode_failed. This guard's shape is narrower: it fires only when an
// encoder re-emits a "begin" line per continuation part, which DecodeUU
// accepts as valid UU on its own terms and which is the shape uuBody below
// stands in for.
func TestDecodePayload_UUPartGreaterThanOneRejected(t *testing.T) {
	t.Parallel()

	got, err := decodePayload([]byte(uuBody), 2)
	if !errors.Is(err, ErrOffsetUnknownForPart) {
		t.Fatalf("decodePayload(part=2) err = %v, want ErrOffsetUnknownForPart", err)
	}
	if got.data != nil {
		t.Errorf("decodePayload(part=2) data = %q, want nil — a rejected decode must not leak a buffer", got.data)
	}
}

// TestDecodePayload_UUFirstSegmentStillDecodes is the companion pin: segment
// 1 is the one segment for which offset 0 is actually correct, and the E5
// guard must not reject it.
func TestDecodePayload_UUFirstSegmentStillDecodes(t *testing.T) {
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

// TestDecodePayload_YencNoYPartRejectedForNonFirstSegment pins the second
// site assertion E5 governs, not just the UU one above: a yEnc body with no
// =ypart line leaves decoder.Article.HasOffset false, so Offset is the
// format's zero-value default rather than genuine position data — the same
// shape as the UU case, from the other decoder. A bare =ybegin part=N does
// not save it: that field is server-declared and unvalidated (D1 in
// docs/article-validation-contract.md only counts a disagreement against the
// NZB's segment number, it does not act on one), so a malformed or lying
// part= cannot be trusted to justify offset 0.
func TestDecodePayload_YencNoYPartRejectedForNonFirstSegment(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		part int
	}{
		{"declares part=5 with no =ypart", 5},
		{"declares no part= at all", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodePayload(yencNoYPart(tc.part, "Hello"), 2)
			if !errors.Is(err, ErrOffsetUnknownForPart) {
				t.Fatalf("decodePayload(requested=2) err = %v, want ErrOffsetUnknownForPart", err)
			}
			if got.data != nil {
				t.Errorf("decodePayload(requested=2) data = %q, want nil", got.data)
			}
		})
	}
}

// TestDecodePayload_YencNoYPartStillDecodesForFirstSegment is the yEnc
// companion to TestDecodePayload_UUFirstSegmentStillDecodes: offset 0 is
// correct when the request really is for segment 1, and the guard must not
// reject that case just because =ypart happens to be absent.
func TestDecodePayload_YencNoYPartStillDecodesForFirstSegment(t *testing.T) {
	t.Parallel()

	got, err := decodePayload(yencNoYPart(0, "Hello"), 1)
	if err != nil {
		t.Fatalf("decodePayload(requested=1): %v", err)
	}
	if string(got.data) != "Hello" {
		t.Errorf("decodePayload(requested=1) data = %q, want %q", got.data, "Hello")
	}
	if got.offset != 0 {
		t.Errorf("decodePayload(requested=1) offset = %d, want 0", got.offset)
	}
}

// TestClassifyDecodeError_OffsetUnknownForPart pins that the E5 rejection
// gets its own telemetry class rather than falling into the generic
// decode_failed bucket used for the yEnc/UU dual-fallback failure. Unlike
// that joined error, this one names a single, specific condition, which is
// what makes a dedicated class actionable where the generic bucket is not.
func TestClassifyDecodeError_OffsetUnknownForPart(t *testing.T) {
	t.Parallel()

	if got := classifyDecodeError(ErrOffsetUnknownForPart); got != telemetry.ErrClassPartOffsetUnknown {
		t.Errorf("classifyDecodeError(ErrOffsetUnknownForPart) = %q, want %q", got, telemetry.ErrClassPartOffsetUnknown)
	}
}

// TestProcessFetchedArticle_OffsetUnknownForPartIsTerminal drives the full
// decode path processFetchedArticle uses in production and pins terminality
// concretely, rather than by name alone: the article is marked emitted (so
// the dispatcher never re-picks it) and counted under its own telemetry
// class rather than the generic decode_failed bucket.
//
// It does NOT assert non-retryability directly — isRetryableDownloaderError
// lives in internal/app and is unexported, so this package cannot call it.
// TestIsRetryableDownloaderError in internal/app/pipeline_test.go pins that
// half, against the same ErrOffsetUnknownForPart sentinel.
func TestProcessFetchedArticle_OffsetUnknownForPartIsTerminal(t *testing.T) {
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, []string{"msg1@h"})
	addTestJob(t, disp, j, m)
	artIdx := artIdxFor(t, disp, j.ID(), "msg1@h")

	d := &Downloader{
		dispatcher:  disp,
		completions: make(chan *ArticleResult, 4),
		log:         slog.New(slog.DiscardHandler),
		tracker:     newDispatchTracker(),
	}
	srv := NewServer(config.ServerConfig{Name: "test-server"})
	req := &articleRequest{
		jobID: j.ID(), fileIdx: 0, artIdx: artIdx, messageID: "msg1@h", partNumber: 2,
	}

	d.processFetchedArticle(t.Context(), srv, req, []byte(uuBody))

	select {
	case res := <-d.completions:
		if !errors.Is(res.Err, ErrOffsetUnknownForPart) {
			t.Errorf("Err = %v, want ErrOffsetUnknownForPart", res.Err)
		}
	default:
		t.Fatal("expected a result to be emitted")
	}
	if got := telemetry.ErrorCount(telemetry.ErrClassPartOffsetUnknown); got != 1 {
		t.Errorf("PipelineErrors[part_offset_unknown] = %d, want 1", got)
	}
	if got := telemetry.ErrorCount(telemetry.ErrClassDecodeFailed); got != 0 {
		t.Errorf("PipelineErrors[decode_failed] = %d, want 0 — the rejection must not double-count into the generic bucket", got)
	}
	if !j.Progress().ArticleEmitted(int(artIdx)) {
		t.Error("article was not marked emitted — a terminal decode error must stop the dispatcher from re-picking it")
	}
}
