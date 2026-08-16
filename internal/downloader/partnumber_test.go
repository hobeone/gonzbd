package downloader

import (
	"fmt"
	"hash/crc32"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// yencPart builds a valid single-part yEnc article declaring part=n, with a
// correct pcrc32 so it decodes cleanly and reaches the success path.
//
// The payload is chosen to contain no byte the yEnc escape rules would expand,
// so the encoded form is a straight +42 of each byte and the fixture stays
// readable.
func yencPart(n int, payload string) []byte {
	encoded := make([]byte, 0, len(payload))
	for i := range len(payload) {
		encoded = append(encoded, payload[i]+42)
	}
	sum := crc32.ChecksumIEEE([]byte(payload))
	return fmt.Appendf(nil,
		"=ybegin part=%d line=128 size=%d name=t.bin\r\n"+
			"=ypart begin=1 end=%d\r\n%s\r\n"+
			"=yend size=%d part=%d pcrc32=%08x\r\n",
		n, len(payload), len(payload), encoded, len(payload), n, sum)
}

// TestDecodePayload_CarriesTheServedPartNumber pins the value surviving the
// decoder boundary. parseHeader tested =ybegin part= only for emptiness and
// threw the value away, so nothing downstream could have compared it.
func TestDecodePayload_CarriesTheServedPartNumber(t *testing.T) {
	got, err := decodePayload(yencPart(3, "Hello"))
	if err != nil {
		t.Fatalf("decodePayload: %v", err)
	}
	if string(got.data) != "Hello" {
		t.Fatalf("decoded %q, want %q — the fixture is not decoding", got.data, "Hello")
	}
	if got.partNumber != 3 {
		t.Errorf("partNumber = %d, want 3 — the served ordinal was discarded, so the "+
			"comparison can never fire", got.partNumber)
	}
}

// TestNotePartNumberDisagreement covers when the probe fires and, more
// importantly, when it does not.
//
// A false positive is not harmless even though nothing acts on the count. The
// whole purpose is to answer "does this case occur in the wild", and a counter
// that ticks on single-part posts or UU articles answers a different question
// with a number that still looks like evidence.
func TestNotePartNumberDisagreement(t *testing.T) {
	tests := []struct {
		name     string
		served   int
		expected int
		wantFire bool
	}{
		{name: "a genuine disagreement is counted", served: 2, expected: 5, wantFire: true},
		{name: "agreement is not", served: 5, expected: 5},
		{name: "an article declaring no part is not, the single-part case", served: 0, expected: 5},
		{name: "an NZB carrying no segment number is not", served: 2, expected: 0},
		{name: "neither side known is not", served: 0, expected: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			telemetry.PartNumberMismatches.Set(0)
			t.Cleanup(func() { telemetry.PartNumberMismatches.Set(0) })
			d := &Downloader{log: slog.New(slog.DiscardHandler)}

			d.notePartNumberDisagreement(&articleRequest{
				jobID: "j", messageID: "m", partNumber: tc.expected,
			}, tc.served)

			want := int64(0)
			if tc.wantFire {
				want = 1
			}
			if got := telemetry.PartNumberMismatches.Value(); got != want {
				t.Errorf("PartNumberMismatches = %d, want %d", got, want)
			}
		})
	}
}

// TestProcessFetchedArticle_PartNumberDisagreementChangesNothing is the
// inaction pin, and the assertion that matters most in this unit.
//
// The comparison is novel — Manifest.ArticleNumber had no consumer before
// this, and SABnzbd never checks the served part against the NZB segment
// number — so nothing establishes how often the two disagree on healthy
// downloads. If a disagreement altered the emitted result or the try-list, an
// indexer that renumbers segments would fail articles across every server at
// once. The probe is only zero-risk while it stays inert.
func TestProcessFetchedArticle_PartNumberDisagreementChangesNothing(t *testing.T) {
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	emit := func(t *testing.T, served, expected int) *ArticleResult {
		t.Helper()
		d := &Downloader{
			queue:       queue.New(),
			completions: make(chan *ArticleResult, 4),
			log:         slog.New(slog.DiscardHandler),
			tracker:     newDispatchTracker(),
		}
		srv := NewServer(config.ServerConfig{Name: "s"})
		req := &articleRequest{
			jobID: "job1", fileIdx: 0, messageID: "msg1", partNumber: expected,
		}

		d.processFetchedArticle(t.Context(), srv, req, yencPart(served, "Hello"))
		select {
		case res := <-d.completions:
			return res
		default:
			t.Fatal("no result emitted")
			return nil
		}
	}

	agreeing := emit(t, 4, 4)
	if agreeing.Err != nil {
		t.Fatalf("baseline emitted an error: %v", agreeing.Err)
	}
	disagreeing := emit(t, 2, 4)

	if disagreeing.Err != nil {
		t.Errorf("Err = %v, want nil — a part-number disagreement became a failure, "+
			"which is exactly the fleet-wide risk this unit refuses to take",
			disagreeing.Err)
	}
	if string(disagreeing.Data) != string(agreeing.Data) {
		t.Errorf("Data = %q, want %q — the payload must be emitted unchanged",
			disagreeing.Data, agreeing.Data)
	}
	if disagreeing.Offset != agreeing.Offset || disagreeing.CRC != agreeing.CRC {
		t.Errorf("emitted result differs beyond the counter: offset=%d crc=%#08x, "+
			"want offset=%d crc=%#08x",
			disagreeing.Offset, disagreeing.CRC, agreeing.Offset, agreeing.CRC)
	}

	// And the probe did observe it, so the assertions above are not passing
	// because the comparison never ran.
	if got := telemetry.PartNumberMismatches.Value(); got != 1 {
		t.Errorf("PartNumberMismatches = %d, want 1 — the disagreement was not "+
			"counted, so this test proves nothing about inaction", got)
	}
}
