//go:build crash && linux

package crash

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/test/mocknntp"
)

// nzbFile is one file's worth of built article data, ready to be written into
// an NZB.
type nzbFile struct {
	Name  string
	Parts [][]byte
	IDs   []string
}

// articleID derives a deterministic Message-ID from (filename, partNum).
func articleID(filename string, partNum int) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s:%d", filename, partNum))
	return fmt.Sprintf("%x", h)[:32] + "@crash.test"
}

// buildArticle returns the Message-ID and yEnc body for one part.
func buildArticle(payload []byte, filename string, partNum, totalParts int, totalSize, offset int64) (messageID string, body []byte) {
	mid := articleID(filename, partNum)
	if totalParts == 1 {
		return mid, mocknntp.EncodeYEnc(filename, payload)
	}
	return mid, mocknntp.EncodeYEncPart(filename, partNum, totalParts, totalSize, offset, payload)
}

// buildNZB renders minimal well-formed NZB XML for the given files.
func buildNZB(files []nzbFile) []byte {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	sb.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")
	now := time.Now().Unix()
	for _, f := range files {
		total := 0
		for _, p := range f.Parts {
			total += len(p)
		}
		subject := fmt.Sprintf("[1/%d] - &quot;%s&quot; yEnc (1/%d) %d",
			len(f.Parts), f.Name, len(f.Parts), total)
		fmt.Fprintf(&sb, "  <file poster=\"poster@crash.test\" date=\"%d\" subject=\"%s\">\n", now, subject)
		sb.WriteString("    <groups><group>alt.test</group></groups>\n")
		sb.WriteString("    <segments>\n")
		for i, part := range f.Parts {
			fmt.Fprintf(&sb, "      <segment bytes=\"%d\" number=\"%d\">%s</segment>\n",
				len(part), i+1, f.IDs[i])
		}
		sb.WriteString("    </segments>\n  </file>\n")
	}
	sb.WriteString("</nzb>\n")
	return []byte(sb.String())
}

// decodeJSON parses an API response body.
func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode API response: %v\nbody: %s", err, b)
	}
	return m
}
