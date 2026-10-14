// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// FileRole identifies the role of a file in a multi-volume Usenet release.
type FileRole string

const (
	// RoleRARVolume represents a split RAR archive volume.
	RoleRARVolume FileRole = "rar_volume"
	// RolePar2Index represents the main PAR2 index file.
	RolePar2Index FileRole = "par2_index"
	// RolePar2Volume represents a PAR2 recovery volume.
	RolePar2Volume FileRole = "par2_volume"
	// RoleSidecar represents non-archive sidecar files like NFO or samples.
	RoleSidecar FileRole = "sidecar"
)

// ExpectedOutput describes a file expected to be extracted from the release.
type ExpectedOutput struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SegmentMeta describes a single NNTP article segment.
type SegmentMeta struct {
	MessageID string `json:"message_id"`
	PartNum   int    `json:"part_num"`
	Offset    int64  `json:"offset"`
	Length    int    `json:"length"`
	CRC32     uint32 `json:"crc32"`
}

// ReleaseFile describes an individual file in the release and its segments.
type ReleaseFile struct {
	FileIndex   int           `json:"file_index"`
	DiskName    string        `json:"disk_name"`
	NZBFilename string        `json:"nzb_filename"`
	Role        FileRole      `json:"role"`
	VolumeNum   int           `json:"volume_num,omitempty"`
	TotalSize   int64         `json:"total_size"`
	SHA256      string        `json:"sha256"`
	Segments    []SegmentMeta `json:"segments"`
}

// ReleaseManifest contains all metadata describing a generated release.
type ReleaseManifest struct {
	Name            string           `json:"name"`
	PartSize        int              `json:"part_size"`
	ExpectedOutputs []ExpectedOutput `json:"expected_outputs"`
	Files           []ReleaseFile    `json:"files"`
}

// SaveManifest serializes a ReleaseManifest as formatted JSON to path.
func SaveManifest(path string, m *ReleaseManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// LoadManifest reads and parses a ReleaseManifest from path.
func LoadManifest(path string) (*ReleaseManifest, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied manifest path
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m ReleaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal manifest: %w", err)
	}
	return &m, nil
}

func xmlEscapeAttr(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("&quot;")
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func xmlEscapeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// BuildNZB generates valid NZB XML from a ReleaseManifest.
func BuildNZB(m *ReleaseManifest) []byte {
	var b bytes.Buffer
	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n")
	b.WriteString("<!DOCTYPE nzb PUBLIC \"-//newzBin//DTD NZB 1.1//EN\" \"http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd\">\n")
	b.WriteString("<nzb xmlns=\"http://www.newzbin.com/DTD/2003/nzb\">\n")
	now := time.Now().Unix()
	for _, f := range m.Files {
		subject := fmt.Sprintf("[1/%d] - %q yEnc (1/%d) %d", len(m.Files), f.NZBFilename, len(f.Segments), f.TotalSize)
		b.WriteString("  <file poster=\"poster@localnntp.test\" date=\"" + strconv.FormatInt(now, 10) + "\" subject=\"" + xmlEscapeAttr(subject) + "\">\n")
		b.WriteString("    <groups>\n")
		b.WriteString("      <group>alt.binaries.test</group>\n")
		b.WriteString("    </groups>\n")
		b.WriteString("    <segments>\n")
		for _, seg := range f.Segments {
			fmt.Fprintf(&b, "      <segment bytes=\"%d\" number=\"%d\">%s</segment>\n",
				seg.Length, seg.PartNum, xmlEscapeText(seg.MessageID))
		}
		b.WriteString("    </segments>\n")
		b.WriteString("  </file>\n")
	}
	b.WriteString("</nzb>\n")
	return b.Bytes()
}
