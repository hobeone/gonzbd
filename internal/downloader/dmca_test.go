package downloader

import (
	"errors"
	"hash/crc32"
	"testing"
)

func TestIsDMCA(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "dmca notice",
			body: "This article has been removed due to a DMCA takedown request.\r\n",
			want: true,
		},
		{
			name: "removed notice",
			body: "Article removed by copyright holder\r\n",
			want: true,
		},
		{
			name: "cancel notice",
			body: "Control: cancel <some-article-id>\r\n",
			want: true,
		},
		{
			name: "blocked notice",
			body: "This content has been blocked.\r\n",
			want: true,
		},
		{
			name: "x-header ignored",
			body: "X-Removed-By: admin\r\nSome regular content here.\r\n",
			want: false,
		},
		{
			name: "yenc header not dmca",
			body: "=ybegin part=1 line=128 size=1024 name=file.bin\r\n",
			want: false,
		},
		{
			name: "empty body",
			body: "",
			want: false,
		},
		{
			name: "regular text",
			body: "Just a normal article body.\r\nNothing to see here.\r\n",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isDMCA([]byte(tc.body))
			if got != tc.want {
				t.Errorf("isDMCA(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestDecodePayload_DMCA(t *testing.T) {
	t.Parallel()

	// A body that is neither yEnc nor UU, but contains DMCA keywords.
	body := []byte("This article has been removed due to a DMCA takedown request.\r\n")

	_, err := decodePayload(body)
	if err == nil {
		t.Fatal("expected error for DMCA body, got nil")
	}
	if !errors.Is(err, ErrArticleRemoved) {
		t.Errorf("expected ErrArticleRemoved, got %v", err)
	}
}

func TestDecodePayload_NonDMCA_NonYenc(t *testing.T) {
	t.Parallel()

	// A body that is neither yEnc nor UU and contains no DMCA keywords.
	body := []byte("Just some random data that is not encoded.\r\n")

	_, err := decodePayload(body)
	if err == nil {
		t.Fatal("expected error for non-encoded body, got nil")
	}
	// Should NOT be ErrArticleRemoved
	if errors.Is(err, ErrArticleRemoved) {
		t.Error("non-DMCA body should not return ErrArticleRemoved")
	}
}

// TestDecodePayload_UUYieldsACRCOverTheDecodedBytes pins the removal of
// ArticleFact.HasCRC.
//
// The durability design records, for every decoded article, a CRC32 over its
// decoded bytes, and a resume verifies the bytes on disk against it. UU is the
// one decode path that used to report no CRC at all — not because the bytes
// are unverifiable, but because the *format* carries no checksum to compare
// against. That is irrelevant here: the fact log verifies our own bytes, not
// the sender's honesty, so the checksum is ours to compute.
//
// With this, no successfully decoded article lacks a CRC, which is the closed
// set that makes dropping the "unverifiable article" branch safe. If this test
// goes red, that branch has to come back.
func TestDecodePayload_UUYieldsACRCOverTheDecodedBytes(t *testing.T) {
	// "Hello" UU-encoded: length char '%' (5 bytes), then two 4-char groups.
	const payload = "Hello"
	body := []byte("begin 644 test.bin\n%2&5L;&\\`\n`\nend\n")

	got, err := decodePayload(body)
	if err != nil {
		t.Fatalf("decodePayload: %v", err)
	}
	if string(got.data) != payload {
		t.Fatalf("decoded %q, want %q — the fixture is not exercising the UU branch", got.data, payload)
	}
	if got.offset != 0 {
		t.Errorf("offset = %d, want 0", got.offset)
	}
	if got.partNumber != 0 {
		t.Errorf("partNumber = %d, want 0 — UU has no part ordinal, and a non-zero "+
			"value here would put every UU article into the disagreement counter",
			got.partNumber)
	}
	want := crc32.ChecksumIEEE(got.data)
	if got.crc != want {
		t.Errorf("crc = %#08x, want %#08x (CRC32 of the decoded UU output). "+
			"A UU article with no CRC cannot be verified on resume, so its bytes "+
			"are re-fetched forever", got.crc, want)
	}
	// A zero CRC here would also satisfy a CRC32-of-empty comparison, so
	// assert the fixture's payload is one whose CRC is not zero.
	if want == 0 {
		t.Fatal("fixture payload has a zero CRC32; the assertion above cannot distinguish it from the old hardcoded 0")
	}
}
