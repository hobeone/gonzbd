package durability

import (
	"context"
	"errors"
	"math/bits"
)

// ErrBitmapTooShort reports a persisted bitmap buffer that cannot hold n bits.
var ErrBitmapTooShort = errors.New("durability: bitmap buffer too short for bit count")

// Bitmap is a fixed-size bit vector of per-article durable flags.
type Bitmap struct {
	words []uint64
	n     int
}

// NewBitmap allocates a Bitmap sized to hold n bits, all initially clear.
func NewBitmap(n int) Bitmap {
	if n < 0 {
		n = 0
	}
	return Bitmap{words: make([]uint64, (n+63)/64), n: n}
}

// Len returns the bit count the Bitmap was created with.
func (b Bitmap) Len() int { return b.n }

// Get reports whether bit i is set. Out-of-range i reports false.
func (b Bitmap) Get(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	return b.words[i/64]&(1<<(uint(i)%64)) != 0
}

// Set mutates the caller's Bitmap despite the value receiver: words is a
// slice, so the receiver copy shares its backing array.
func (b Bitmap) Set(i int) {
	if i < 0 || i >= b.n {
		return
	}
	b.words[i/64] |= 1 << (uint(i) % 64)
}

// Count returns the number of set bits.
func (b Bitmap) Count() int {
	total := 0
	for _, w := range b.words {
		total += bits.OnesCount64(w)
	}
	return total
}

// Bytes serialises the bitmap little-endian for persistence.
func (b Bitmap) Bytes() []byte {
	out := make([]byte, len(b.words)*8)
	for i, w := range b.words {
		for j := range 8 {
			out[i*8+j] = byte(w >> (8 * uint(j))) //nolint:gosec // G115: intentional truncation to extract one byte of w
		}
	}
	return out
}

// BitmapFromBytes rebuilds a bitmap, rejecting a buffer too short for n.
// A short buffer means the record is damaged; silently returning a partly
// zero bitmap would report those articles not-durable without anyone
// noticing the damage, which S3 forbids treating as an ordinary answer.
func BitmapFromBytes(buf []byte, n int) (Bitmap, error) {
	if n < 0 {
		n = 0
	}
	need := (n + 63) / 64
	if len(buf) < need*8 {
		return Bitmap{}, ErrBitmapTooShort
	}
	b := NewBitmap(n)
	for i := range need {
		var w uint64
		for j := range 8 {
			w |= uint64(buf[i*8+j]) << (8 * uint(j))
		}
		b.words[i] = w
	}
	return b, nil
}

// FileExtent is the Class B derivation cache for one file. Every field is
// recomputable from the FactLog plus the file's bytes; none is authoritative.
type FileExtent struct {
	FileIdx int32
	// Durable has one bit per article of this file, in file-local ordinal
	// order, set when a completed fsync covered that article's bytes.
	Durable Bitmap
	// VerifiedTo is the length of the gapless prefix proven present from
	// byte 0. It is the CRC anchor, and is permitted to stall at a hole
	// without affecting resume, which depends only on Durable.
	VerifiedTo int64
	// PrefixCRC is the CRC32 of [0, VerifiedTo), valid only when HasPrefixCRC.
	PrefixCRC    uint32
	HasPrefixCRC bool
	// BytesDurable and BytesFailed are cached aggregates. They exist so
	// restart stays O(files) (B3) rather than O(articles); the FactLog
	// remains the authority (S5).
	BytesDurable int64
	BytesFailed  int64
	// Size and ModTimeNs stamp the file as it was at commit time. The
	// resumer compares them against the file as it exists now; a mismatch
	// invalidates every other field in this struct (S7).
	Size      int64
	ModTimeNs int64
}

// ExtentStore persists Class B. Commit is atomic across the whole slice so a
// job's files can never be observed half-committed.
type ExtentStore interface {
	Commit(ctx context.Context, jobID string, exts []FileExtent) error
	Load(ctx context.Context, jobID string) ([]FileExtent, error)
	DeleteJob(ctx context.Context, jobID string) error
}
