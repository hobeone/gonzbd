#!/usr/bin/env python3
"""Generate internal/i18n/english.go from upstream sabnzbd/sabnzbd/skintext.py.

The upstream SKIN_TEXT dict wraps every value in TT("..."), a
gettext-extraction marker (TT = lambda x: x). We strip the TT wrapper and
emit the contents as a Go map literal returned by DefaultEnglish().

Run from the sabnzbd-go repo root. Upstream source is expected at
../sabnzbd/sabnzbd/skintext.py (the sibling Python checkout).

After running, gofmt the output to get the map-literal column alignment:

    python3 scripts/gen_english_catalog.py > internal/i18n/english.go
    gofmt -w internal/i18n/english.go
"""

from __future__ import annotations

import ast
import sys
from pathlib import Path

SRC = Path("../sabnzbd/sabnzbd/skintext.py")


def go_quote(s: str) -> str:
    """Return s as a Go double-quoted string literal with escapes.

    Go's double-quoted strings accept \\n, \\t, \\", \\\\, and \\xNN for
    other control bytes. That covers every character seen in SKIN_TEXT.
    """
    out = ['"']
    for ch in s:
        if ch == "\\":
            out.append("\\\\")
        elif ch == '"':
            out.append('\\"')
        elif ch == "\n":
            out.append("\\n")
        elif ch == "\t":
            out.append("\\t")
        elif ch == "\r":
            out.append("\\r")
        elif ord(ch) < 0x20:
            out.append(f"\\x{ord(ch):02x}")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def extract_skin_text(src_path: Path) -> ast.Dict:
    tree = ast.parse(src_path.read_text())
    for node in ast.walk(tree):
        if isinstance(node, ast.Assign):
            for tgt in node.targets:
                if isinstance(tgt, ast.Name) and tgt.id == "SKIN_TEXT":
                    if not isinstance(node.value, ast.Dict):
                        raise SystemExit("SKIN_TEXT is not a dict literal")
                    return node.value
    raise SystemExit("SKIN_TEXT assignment not found")


def main() -> int:
    if not SRC.exists():
        print(f"error: {SRC} not found (expected sibling Python checkout)", file=sys.stderr)
        return 1
    skin = extract_skin_text(SRC)

    out = sys.stdout
    out.write("package i18n\n\n")
    out.write("// This file is generated from upstream sabnzbd/sabnzbd/skintext.py.\n")
    out.write("// Regenerate via scripts/gen_english_catalog.py — do not hand-edit.\n")
    out.write("//\n")
    out.write('// The upstream SKIN_TEXT dict wraps each value in TT("..."), a\n')
    out.write("// gettext-extraction marker (TT = lambda x: x). For the Go port the\n")
    out.write("// TT wrapper is stripped and the English value becomes the default\n")
    out.write("// catalog entry.\n\n")
    out.write("// DefaultEnglish returns the English translation catalog ported from\n")
    out.write("// upstream SABnzbd's skintext.SKIN_TEXT. It is the v1 default catalog\n")
    out.write("// used by the production web handler; future localization will layer\n")
    out.write("// .po-loaded catalogs on top of this.\n")
    out.write("func DefaultEnglish() Catalog {\n")
    # gosec G101: the dense "key": "value" pattern trips the hardcoded-credentials
    # heuristic — false positive for a translation catalog.
    # misspell: upstream skintext.py has typos (e.g. "untill", "Seperate") that
    # have shipped for years; re-ports must preserve them verbatim to keep the
    # Go catalog a faithful mirror of the Python source-of-truth.
    out.write("\t//nolint:gosec,misspell // generated from upstream skintext.SKIN_TEXT; values preserved verbatim, not credentials\n")
    out.write("\treturn Catalog{\n")

    count = 0
    for k, v in zip(skin.keys, skin.values):
        if not isinstance(k, ast.Constant) or not isinstance(k.value, str):
            raise SystemExit(f"non-string SKIN_TEXT key near line {getattr(k, 'lineno', '?')}")
        if not (
            isinstance(v, ast.Call)
            and isinstance(v.func, ast.Name)
            and v.func.id == "TT"
            and len(v.args) == 1
            and isinstance(v.args[0], ast.Constant)
            and isinstance(v.args[0].value, str)
        ):
            raise SystemExit(f"value for key {k.value!r} is not TT(\"...\") with a single string literal")
        out.write(f"\t\t{go_quote(k.value)}: {go_quote(v.args[0].value)},\n")
        count += 1

    out.write("\t}\n")
    out.write("}\n")
    print(f"# wrote {count} entries", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
