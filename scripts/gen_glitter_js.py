#!/usr/bin/env python3
"""Assemble internal/web/static/glitter/javascripts/glitter.js from upstream sub-files.

Upstream's glitter.js is a Cheetah template that uses `#include raw` to splice
five sub-files at server-render time:

    /******
            Glitter V1 ... (docstring preamble)
    ******/

    #include raw $webdir + "/static/javascripts/glitter.basic.js"#

    /**
        GLITTER CODE
    **/
    \\$(function() {
        'use strict';

        #include raw $webdir + "/static/javascripts/glitter.main.js"#
        #include raw $webdir + "/static/javascripts/glitter.queue.js"#
        #include raw $webdir + "/static/javascripts/glitter.history.js"#
        #include raw $webdir + "/static/javascripts/glitter.filelist.pagination.js"#

        // GO!!!
        ko.applyBindings(new ViewModel(), document.getElementById("sabnzbd"));
    });

Our Go port has no Cheetah engine at runtime, so we pre-assemble once and
serve the resulting plain JavaScript. The layout below mirrors upstream so
browsers see the same bytes they would against a Python SABnzbd install.

The `\\$(` literal on line 46 of the upstream template is Cheetah's way of
emitting a bare `$` (which would otherwise trigger variable interpolation).
After rendering it becomes `$(` — the jQuery ready shorthand.

Run from the sabnzbd-go repo root. Upstream is expected at ../sabnzbd.
"""

from __future__ import annotations

import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
UPSTREAM_JS_DIR = REPO_ROOT / "../sabnzbd/interfaces/Glitter/templates/static/javascripts"
OUTPUT = REPO_ROOT / "internal/web/static/glitter/javascripts/glitter.js"

# Sub-files that are included outside the $(function(){...}) wrapper.
OUTER = ["glitter.basic.js"]

# Sub-files that are included inside the wrapper (after 'use strict'; before
# the ko.applyBindings tail). Order mirrors upstream.
INNER = [
    "glitter.main.js",
    "glitter.queue.js",
    "glitter.history.js",
    "glitter.filelist.pagination.js",
]

PREAMBLE = """/******

        Glitter V1
        By Safihre (2015) - safihre@sabnzbd.org

        Code extended from Shiny-template
        Code examples used from Knockstrap-template

        The setup is hierarchical, 1 main ViewModel that contains:
        - ViewModel
            - QueueListModel
                - paginationModel
                - QueueModel (item 1)
                - QueueModel (item 2)
                - ...
                - QueueModel (item n+1)
            - HistoryListModel
                - paginationModel
                - HistoryModel (item 1)
                - HistoryModel (item 2)
                - ...
                - HistoryModel (item n+1)
            - Fileslisting
                - FileslistingModel (file 1)
                - FileslistingModel (file 2)
                - ...
                - FileslistingModel (file n+1)

        ViewModel also contains all the code executed on document ready and
        functions responsible for the status information, adding NZB, etc.
        The QueueModel/HistoryModel's get added to the list-models when
        jobs are added or on switching of pages (using paginationModel).
        Once added only the properties that changed during a refresh
        get updated. In the history all the detailed information is only
        updated when created and when the user clicks on a detail.
        The Fileslisting is only populated and updated when it is opened
        for one of the QueueModel's.

******/
"""

MIDDLE_COMMENT = """/**
    GLITTER CODE
**/
"""

ASSEMBLY_HEADER = """// This file is generated — do not hand-edit.
// Regenerate via scripts/gen_glitter_js.py.
//
// Source of truth: upstream sabnzbd/interfaces/Glitter/templates/static/javascripts/
// glitter.js (a Cheetah template with raw-include directives). Python SABnzbd
// expands those includes at render time; the Go port pre-assembles equivalently
// so the browser receives plain JavaScript.

"""


def read(fname: str) -> str:
    path = UPSTREAM_JS_DIR / fname
    if not path.exists():
        raise SystemExit(f"error: upstream sub-file {path} not found")
    text = path.read_text()
    if not text.endswith("\n"):
        text += "\n"
    return text


def main() -> int:
    if not UPSTREAM_JS_DIR.exists():
        print(f"error: upstream JS dir {UPSTREAM_JS_DIR} not found", file=sys.stderr)
        return 1

    parts: list[str] = [ASSEMBLY_HEADER, PREAMBLE, "\n"]
    for name in OUTER:
        parts.append(read(name))
        parts.append("\n")

    parts.append(MIDDLE_COMMENT)
    parts.append("$(function() {\n")
    parts.append("    'use strict';\n\n")
    for name in INNER:
        parts.append(read(name))
        parts.append("\n")
    parts.append("    // GO!!!\n")
    parts.append('    ko.applyBindings(new ViewModel(), document.getElementById("sabnzbd"));\n')
    parts.append("});\n")

    OUTPUT.write_text("".join(parts))
    print(f"# wrote {OUTPUT} ({OUTPUT.stat().st_size} bytes)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
