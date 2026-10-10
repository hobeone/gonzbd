#!/usr/bin/env bash
#
# gen_test_archives.sh — Generate test RAR archives for internal/unpack/testdata/
#
# Prerequisites: rar (WinRAR CLI >= 7.0), unrar
# Run from the repo root: ./scripts/gen_test_archives.sh
#
# NOTE: RAR 7.x only creates RAR5 archives (-ma4 removed). RAR4 test fixtures
# must be generated with an older rar binary and committed separately.
#
set -euo pipefail

OUTDIR="internal/unpack/testdata"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

# Clean previous output
# rar5_link_symlink/hard/solid.rar are copied from rarengine's testdata, not
# generated here, so the clean step leaves them alone.
find "$OUTDIR" -maxdepth 1 \( -name '*.rar' -o -name '*.r[0-9][0-9]' \) \
	! -name 'rar5_link_symlink.rar' ! -name 'rar5_link_hard.rar' ! -name 'rar5_link_solid.rar' -delete
mkdir -p "$OUTDIR"

# Create source content files
echo "Hello from file1.txt" > "$TMPDIR/file1.txt"
echo "Hello from file2.txt" > "$TMPDIR/file2.txt"
mkdir -p "$TMPDIR/subdir"
echo "Nested file content"  > "$TMPDIR/subdir/nested.txt"

# --- Single-volume RAR5, no password ---
echo "==> single_rar5.rar"
( cd "$TMPDIR" && rar a -r "$(cd - >/dev/null && pwd)/$OUTDIR/single_rar5.rar" file1.txt file2.txt subdir/ >/dev/null )

# compressed_rar5.rar: the one fixture here whose member is compressed rather
# than stored, so it exercises rarengine's block decoder (and its worker
# pipeline). The plaintext is regenerated line for line by
# compressedFixtureText in internal/unpack/decode_workers_test.go.
echo "==> compressed_rar5.rar"
for i in $(seq 0 5999); do
	printf 'line %06d the quick brown fox jumps over the lazy dog\n' "$i"
done > "$TMPDIR/compressed.txt"
( cd "$TMPDIR" && rar a -ma5 -ep -m3 -md128k "$(cd - >/dev/null && pwd)/$OUTDIR/compressed_rar5.rar" compressed.txt >/dev/null )

# --- Multi-volume RAR5, new naming (partNN.rar) ---
# Create a larger file to force multi-volume with 1KB volumes.
dd if=/dev/urandom of="$TMPDIR/bigfile.bin" bs=1024 count=8 2>/dev/null
echo "==> multi_new.partNN.rar"
( cd "$TMPDIR" && rar a -v1k "$(cd - >/dev/null && pwd)/$OUTDIR/multi_new.rar" bigfile.bin >/dev/null )
# rar creates multi_new.part1.rar, multi_new.part2.rar, etc.
echo "   Created: $(ls "$OUTDIR"/multi_new.part*.rar 2>/dev/null | tr '\n' ' ')"

# --- Password-protected RAR5 ---
echo "==> password_rar5.rar (password: 'testpass')"
( cd "$TMPDIR" && rar a -ptestpass "$(cd - >/dev/null && pwd)/$OUTDIR/password_rar5.rar" file1.txt >/dev/null )

# --- Encrypted-header RAR5 (password required to even list) ---
echo "==> encrypted_header.rar (password: 'testpass')"
( cd "$TMPDIR" && rar a -hp"testpass" "$(cd - >/dev/null && pwd)/$OUTDIR/encrypted_header.rar" file1.txt >/dev/null )

# --- Corrupt RAR (take a valid RAR and overwrite bytes in the middle) ---
echo "==> corrupt.rar"
cp "$OUTDIR/single_rar5.rar" "$OUTDIR/corrupt.rar"
printf '\x00\x00\x00\x00\x00\x00\x00\x00' | dd of="$OUTDIR/corrupt.rar" bs=1 seek=20 conv=notrunc 2>/dev/null

# --- Archive with directory entries (for IsDir handling) ---
echo "==> with_dirs.rar"
( cd "$TMPDIR" && rar a -r "$(cd - >/dev/null && pwd)/$OUTDIR/with_dirs.rar" subdir/ >/dev/null )

# --- Hostile symlink: target climbs out of the extraction root ---
echo "==> rar5_link_escape.rar"
mkdir -p "$TMPDIR/escape"
( cd "$TMPDIR/escape" && echo "real" > real.txt && ln -s ../../etc/passwd evil.lnk && echo "ok" > after.txt \
	&& rar a -ol -ep "$(cd - >/dev/null && pwd)/$OUTDIR/rar5_link_escape.rar" real.txt evil.lnk after.txt >/dev/null )

# --- File reference: copy.txt stored as a reference to identical orig.txt ---
echo "==> rar5_link_filecopy.rar"
mkdir -p "$TMPDIR/filecopy"
( cd "$TMPDIR/filecopy" && printf 'file copy content, stored once and referenced by copy.txt\n' > orig.txt && cp orig.txt copy.txt \
	&& rar a -ma5 -oi:1 -m0 "$(cd - >/dev/null && pwd)/$OUTDIR/rar5_link_filecopy.rar" orig.txt copy.txt >/dev/null )

echo ""
echo "=== Generated test archives ==="
ls -la "$OUTDIR"/*.rar "$OUTDIR"/*.r[0-9][0-9] 2>/dev/null || true
echo ""
echo "Done. Commit the testdata/ directory."
