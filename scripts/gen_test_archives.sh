#!/usr/bin/env bash
#
# gen_test_archives.sh — Generate test RAR archives for internal/unpack/testdata/
#
# Prerequisites: rar (WinRAR CLI >= 7.0), unrar, 7z (for the flat_*.7z fixtures)
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

# --- Multi-volume RAR5 with two members ---
# first.txt ends in volume 1 and second.txt spans the rest, so a damaged later
# volume fails the set after its first member is already extracted. Stored
# (-m0), so a byte flipped in a middle volume lands in second.txt's data.
echo "==> two_members.partN.rar"
echo "first member, complete in volume 1" > "$TMPDIR/first.txt"
for i in $(seq 0 199); do
	printf 'second member line %04d\n' "$i"
done > "$TMPDIR/second.txt"
( cd "$TMPDIR" && rar a -ma5 -m0 -ep -v1k "$(cd - >/dev/null && pwd)/$OUTDIR/two_members.rar" first.txt second.txt >/dev/null )

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

# --- Flattening collisions (flat_unpack), as RAR5 and 7z (#897) ---
# flat_collide: a/f.txt="one" and b/f.txt="two" flatten to one name.
# flat_dir_file: file a/x and directory b/x (holding b/x/y) flatten to one
# name. rar stores the file member before the directory; 7z stores the
# directories first.
echo "==> flat_collide.{rar,7z} flat_dir_file.{rar,7z}"
mkdir -p "$TMPDIR/collide/a" "$TMPDIR/collide/b" "$TMPDIR/dirfile/a" "$TMPDIR/dirfile/b/x"
printf 'one' > "$TMPDIR/collide/a/f.txt"
printf 'two' > "$TMPDIR/collide/b/f.txt"
printf 'file' > "$TMPDIR/dirfile/a/x"
printf 'y' > "$TMPDIR/dirfile/b/x/y"
rm -f "$OUTDIR/flat_collide.7z" "$OUTDIR/flat_dir_file.7z"
( cd "$TMPDIR/collide" && rar a -ma5 -r "$(cd - >/dev/null && pwd)/$OUTDIR/flat_collide.rar" a b >/dev/null \
	&& 7z a -bd "$(cd - >/dev/null && pwd)/$OUTDIR/flat_collide.7z" a b >/dev/null )
( cd "$TMPDIR/dirfile" && rar a -ma5 -r "$(cd - >/dev/null && pwd)/$OUTDIR/flat_dir_file.rar" a b >/dev/null \
	&& 7z a -bd "$(cd - >/dev/null && pwd)/$OUTDIR/flat_dir_file.7z" a b >/dev/null )

echo ""
echo "=== Generated test archives ==="
ls -la "$OUTDIR"/*.rar "$OUTDIR"/*.r[0-9][0-9] 2>/dev/null || true
echo ""
echo "Done. Commit the testdata/ directory."
