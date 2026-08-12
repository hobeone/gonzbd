package durability

import (
	"context"
	"encoding/binary"
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
		binary.LittleEndian.PutUint64(out[i*8:], w)
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
		b.words[i] = binary.LittleEndian.Uint64(buf[i*8:])
	}
	// Mask the padding bits of the final word. A persisted buffer may be
	// damaged, and this constructor exists to read exactly that input class —
	// the short-buffer guard above defends the same case. Without the mask,
	// garbage above bit n is absorbed: Get is bounded by n but Count is not,
	// so Count could exceed Len and over-report how many articles are
	// durable. Over-counting is the over-claim direction the design forbids.
	if rem := n % 64; rem != 0 && need > 0 {
		b.words[need-1] &= (1 << uint(rem)) - 1
	}
	return b, nil
}

// FileExtent is the Class B derivation cache for one file. Every field is
// recomputable from the FactLog plus the file's bytes, and none is
// authoritative: where one disagrees with a recomputation, the recomputation
// is correct by definition (S4).
//
// That claim holds without exception, which is why there is no failed-byte
// field here. A permanently failed article never decodes, so it never writes
// an ArticleFact, and no recomputation from Class A could reproduce such a
// figure — it would be the one field S4 could not be applied to. It is cached
// in job_files.failed_bytes instead, beside the articles_done bits it sums.
type FileExtent struct {
	FileIdx int32
	// Durable has one bit per article of this file, in file-local ordinal
	// order, set when a completed fsync covered that article's bytes.
	Durable Bitmap
	// VerifiedTo is the length of the gapless prefix proven present from
	// byte 0. It is the CRC anchor, and is permitted to stall at a hole
	// without affecting resume, which depends only on Durable.
	VerifiedTo int64
	// PrefixCRC is the CRC32 of exactly [0, VerifiedTo). That range holds
	// whether or not HasPrefixCRC is set, so the flag must not be read as
	// describing what the CRC covers.
	PrefixCRC uint32
	// HasPrefixCRC means "PrefixCRC is a verified WHOLE-FILE CRC", not
	// merely "PrefixCRC is populated". It is set only when the verification
	// run consumed every recorded fact for the file AND the gapless prefix
	// reached the file's end. Anything less is unavailable (R23) — an honest
	// answer, and one that must stay distinguishable from a CRC of zero.
	//
	// The loose reading, "the CRC of whatever prefix we have", is what lets a
	// partial extent's CRC be reported as the file's. That is #349, and it is
	// why Resumer re-checks this flag against the file's current size rather
	// than adopting a committed one: the flag can outlive its condition when
	// a file grows past a hole without VerifiedTo moving.
	HasPrefixCRC bool
	// BytesDurable is a cached aggregate. It exists so restart stays
	// O(files) (B3) rather than O(articles).
	//
	// It summarises the FactLog, which remains authoritative for it (S5):
	// every byte counted here has an ArticleFact behind it, so a
	// recomputation from Class A plus the file's bytes reproduces the figure
	// and supersedes this cache when the two differ.
	BytesDurable int64
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
