package downloader

import (
	"errors"
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

	_, _, err := decodePayload(body)
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

	_, _, err := decodePayload(body)
	if err == nil {
		t.Fatal("expected error for non-encoded body, got nil")
	}
	// Should NOT be ErrArticleRemoved
	if errors.Is(err, ErrArticleRemoved) {
		t.Error("non-DMCA body should not return ErrArticleRemoved")
	}
}
