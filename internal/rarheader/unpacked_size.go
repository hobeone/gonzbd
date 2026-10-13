package rarheader

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrSizeUnknown is returned by UnpackedSize for an archive whose headers do
// not state its unpacked size: headers encrypted, a member whose header
// declares its size unknown, or an archive that does not start with a RAR
// signature (a self-extracting one, for instance).
var ErrSizeUnknown = errors.New("rarheader: unpacked size not declared")

// maxBlockHeader bounds the header bytes UnpackedSize reads for one block.
// It is the largest header the RAR5 format can describe (three-byte vint).
const maxBlockHeader = 2 << 20

// UnpackedSize returns the sum of the unpacked sizes the file headers of a
// RAR3 or RAR5 archive declare, over volumes given in playback order. A
// member split across volumes is counted once, from the header that starts
// it; directories count nothing. It reads block headers only and seeks over
// packed data, so its cost does not grow with the archive's content.
//
// It returns ErrSizeUnknown when the headers do not state the size, and
// another error when a volume cannot be read or its headers are damaged.
func UnpackedSize(volumes []string) (int64, error) {
	var total int64
	for _, v := range volumes {
		n, err := volumeUnpackedSize(v)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func volumeUnpackedSize(p string) (n int64, err error) {
	f, err := os.Open(p) //nolint:gosec // read-only open of a caller-supplied archive volume
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	ver, err := readMagic(p)
	if errors.Is(err, ErrNotRAR) {
		return 0, ErrSizeUnknown
	}
	if err != nil {
		return 0, err
	}
	if ver == 5 {
		n, err = rar5UnpackedSize(f)
	} else {
		n, err = rar3UnpackedSize(f)
	}
	if err != nil && !errors.Is(err, ErrSizeUnknown) {
		err = fmt.Errorf("rarheader: %s: %w", p, err)
	}
	return n, err
}

// RAR5 block and file-header flags (RAR 5.0 archive format, "General
// archive block format" and "File header and service header").
const (
	rar5TypeFile       = 2
	rar5TypeEncryption = 4
	rar5TypeEnd        = 5

	rar5FlagExtra        = 0x0001
	rar5FlagData         = 0x0002
	rar5FlagDataNotFirst = 0x0008

	rar5FileDir         = 0x0001
	rar5FileSizeUnknown = 0x0008
)

func rar5UnpackedSize(f *os.File) (int64, error) {
	off := int64(len(rar5Sig))
	var total int64
	for {
		hdr, next, err := rar5Block(f, off)
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return 0, err
		}
		switch hdr.typ {
		case rar5TypeEncryption:
			return 0, ErrSizeUnknown
		case rar5TypeEnd:
			return total, nil
		case rar5TypeFile:
			if hdr.flags&rar5FlagDataNotFirst == 0 {
				size, err := rar5FileSize(hdr.body)
				if err != nil {
					return 0, err
				}
				total += size
			}
		}
		off = next
	}
}

type rar5Header struct {
	typ, flags uint64
	body       []byte // the type-specific fields that follow the common ones
}

// rar5Block reads the block header at off and returns it with the offset of
// the next block.
func rar5Block(f *os.File, off int64) (rar5Header, int64, error) {
	br := bufio.NewReader(io.NewSectionReader(f, off, maxBlockHeader+16))
	var crc [4]byte
	if _, err := io.ReadFull(br, crc[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return rar5Header{}, 0, io.EOF
		}
		return rar5Header{}, 0, err
	}
	size, sizeLen, err := readVint(br)
	if err != nil {
		return rar5Header{}, 0, err
	}
	if size == 0 || size > maxBlockHeader {
		return rar5Header{}, 0, fmt.Errorf("block at %d: header size %d out of range", off, size)
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(br, raw); err != nil {
		return rar5Header{}, 0, fmt.Errorf("block at %d: %w", off, err)
	}
	rest := raw
	var hdr rar5Header
	if hdr.typ, rest, err = vint(rest); err != nil {
		return rar5Header{}, 0, err
	}
	if hdr.flags, rest, err = vint(rest); err != nil {
		return rar5Header{}, 0, err
	}
	if hdr.flags&rar5FlagExtra != 0 {
		if _, rest, err = vint(rest); err != nil {
			return rar5Header{}, 0, err
		}
	}
	var data uint64
	if hdr.flags&rar5FlagData != 0 {
		if data, rest, err = vint(rest); err != nil {
			return rar5Header{}, 0, err
		}
	}
	hdr.body = rest
	next := off + 4 + int64(sizeLen) + int64(size) //nolint:gosec // size <= maxBlockHeader
	if data > uint64(1)<<62 {
		return rar5Header{}, 0, fmt.Errorf("block at %d: data size %d out of range", off, data)
	}
	return hdr, next + int64(data), nil //nolint:gosec // bounded above
}

// rar5FileSize reads the unpacked size from a file header's own fields.
func rar5FileSize(body []byte) (int64, error) {
	fileFlags, rest, err := vint(body)
	if err != nil {
		return 0, err
	}
	size, _, err := vint(rest)
	if err != nil {
		return 0, err
	}
	if fileFlags&rar5FileSizeUnknown != 0 {
		return 0, ErrSizeUnknown
	}
	if fileFlags&rar5FileDir != 0 {
		return 0, nil
	}
	if size > uint64(1)<<62 {
		return 0, fmt.Errorf("unpacked size %d out of range", size)
	}
	return int64(size), nil //nolint:gosec // bounded above
}

// readVint reads one RAR5 variable-length integer from r, returning it and
// its length in bytes.
func readVint(r io.ByteReader) (v uint64, n int, err error) {
	for i := range 10 {
		var b byte
		b, err = r.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		v |= uint64(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errors.New("vint longer than 10 bytes")
}

// vint reads one RAR5 variable-length integer from the front of b.
func vint(b []byte) (v uint64, rest []byte, err error) {
	r := byteSliceReader{b: b}
	v, n, err := readVint(&r)
	if err != nil {
		return 0, nil, fmt.Errorf("truncated header field: %w", err)
	}
	return v, b[n:], nil
}

type byteSliceReader struct {
	b []byte
	i int
}

func (r *byteSliceReader) ReadByte() (byte, error) {
	if r.i >= len(r.b) {
		return 0, io.ErrUnexpectedEOF
	}
	c := r.b[r.i]
	r.i++
	return c, nil
}

// RAR 1.5-4.x block types and flags (technote.txt of RAR 3.x).
const (
	rar3TypeMain = 0x73
	rar3TypeFile = 0x74
	rar3TypeSub  = 0x7a
	rar3TypeEnd  = 0x7b

	rar3LongBlock       = 0x8000
	rar3MainPassword    = 0x0080
	rar3FileSplitBefore = 0x0001
	rar3FileDirMask     = 0x00e0
	rar3FileLarge       = 0x0100

	rar3BaseHeader = 7  // HEAD_CRC, HEAD_TYPE, HEAD_FLAGS, HEAD_SIZE
	rar3FileFixed  = 25 // PACK_SIZE .. ATTR after the base header
)

func rar3UnpackedSize(f *os.File) (int64, error) {
	off := int64(len(rar3Sig))
	var total int64
	buf := make([]byte, rar3BaseHeader+rar3FileFixed+8)
	for {
		n, err := f.ReadAt(buf, off)
		if n < rar3BaseHeader {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		typ := buf[2]
		flags := binary.LittleEndian.Uint16(buf[3:5])
		headSize := int64(binary.LittleEndian.Uint16(buf[5:7]))
		if headSize < rar3BaseHeader {
			return 0, fmt.Errorf("block at %d: header size %d out of range", off, headSize)
		}
		var data int64
		switch typ {
		case rar3TypeMain:
			if flags&rar3MainPassword != 0 {
				return 0, ErrSizeUnknown
			}
		case rar3TypeEnd:
			return total, nil
		case rar3TypeFile, rar3TypeSub:
			fixed := rar3BaseHeader + rar3FileFixed
			if flags&rar3FileLarge != 0 {
				fixed += 8
			}
			if n < fixed || headSize < int64(fixed) {
				return 0, fmt.Errorf("block at %d: file header truncated", off)
			}
			packed := int64(binary.LittleEndian.Uint32(buf[7:11]))
			unpacked := int64(binary.LittleEndian.Uint32(buf[11:15]))
			if flags&rar3FileLarge != 0 {
				packed |= int64(binary.LittleEndian.Uint32(buf[32:36])) << 32
				unpacked |= int64(binary.LittleEndian.Uint32(buf[36:40])) << 32
			}
			data = packed
			if typ == rar3TypeFile && flags&rar3FileSplitBefore == 0 && flags&rar3FileDirMask != rar3FileDirMask {
				total += unpacked
			}
		default:
			if flags&rar3LongBlock != 0 {
				if n < rar3BaseHeader+4 {
					return 0, fmt.Errorf("block at %d: header truncated", off)
				}
				data = int64(binary.LittleEndian.Uint32(buf[7:11]))
			}
		}
		off += headSize + data
	}
}
