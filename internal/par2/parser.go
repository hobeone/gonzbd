package par2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// FileDesc contains metadata about a file protected by a PAR2 set.
type FileDesc struct {
	FileName string
	Hash16k  [16]byte
}

var (
	magic        = []byte("PAR2\x00\x00\x00\x00")
	typeFileDesc = [16]byte{'P', 'A', 'R', ' ', '2', '.', '0', '\x00', 'F', 'i', 'l', 'e', 'D', 'e', 's', 'c'}
)

// ParseFileDescriptions reads path (a .par2 file) and returns all File Description
// packets found within.
func ParseFileDescriptions(path string) ([]FileDesc, error) {
	f, err := os.Open(path) //nolint:gosec // path is constructed from trusted readdir
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := uint64(fi.Size())

	var descs []FileDesc

	for {
		// Read 64-byte packet header.
		header := make([]byte, 64)
		_, err := io.ReadFull(f, header)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read header: %w", err)
		}

		if !bytes.Equal(header[0:8], magic) {
			// Not a packet start? The spec says packets can be preceded by junk,
			// but in practice they are usually contiguous.
			// Let's try to find the next magic if this one didn't match.
			// For now, if we hit junk we'll just stop to be safe/efficient.
			break
		}

		packetLen := binary.LittleEndian.Uint64(header[8:16])
		if packetLen < 64 || packetLen > fileSize {
			return nil, fmt.Errorf("invalid packet length: %d", packetLen)
		}

		var packetType [16]byte
		copy(packetType[:], header[48:64])

		if packetType == typeFileDesc {
			// File Description Packet body starts at offset 64.
			// FileID (16) @ 64
			// HashFull (16) @ 80
			// Hash16k (16) @ 96
			// FileLength (8) @ 112
			// FileName (var) @ 120

			bodyLen := packetLen - 64
			body := make([]byte, bodyLen)
			if _, err := io.ReadFull(f, body); err != nil {
				return nil, fmt.Errorf("read body: %w", err)
			}

			if bodyLen < 56 {
				continue
			}

			var hash16k [16]byte
			copy(hash16k[:], body[32:48]) // 96 - 64 = 32

			fileName := string(bytes.TrimRight(body[56:], "\x00"))
			descs = append(descs, FileDesc{
				FileName: fileName,
				Hash16k:  hash16k,
			})
		} else {
			// Skip this packet.
			if _, err := f.Seek(int64(packetLen-64), io.SeekCurrent); err != nil { //nolint:gosec // packetLen validated > 64
				return nil, fmt.Errorf("seek: %w", err)
			}
		}
	}

	return descs, nil
}
