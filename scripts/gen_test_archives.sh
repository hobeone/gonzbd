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
rm -f "$OUTDIR"/*.rar "$OUTDIR"/*.r[0-9][0-9]
mkdir -p "$OUTDIR"

# Create source content files
echo "Hello from file1.txt" > "$TMPDIR/file1.txt"
echo "Hello from file2.txt" > "$TMPDIR/file2.txt"
mkdir -p "$TMPDIR/subdir"
echo "Nested file content"  > "$TMPDIR/subdir/nested.txt"

# --- Single-volume RAR5, no password ---
echo "==> single_rar5.rar"
( cd "$TMPDIR" && rar a -r "$(cd - >/dev/null && pwd)/$OUTDIR/single_rar5.rar" file1.txt file2.txt subdir/ >/dev/null )

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

echo ""
echo "=== Generated test archives ==="
ls -la "$OUTDIR"/*.rar "$OUTDIR"/*.r[0-9][0-9] 2>/dev/null || true
echo ""
echo "Done. Commit the testdata/ directory."
