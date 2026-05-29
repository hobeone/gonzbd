# Post-Processing Scripts

GoNZBD can run a user-supplied script after each download completes. Scripts
are invoked once per job, after all post-processing stages (repair, unpack,
deobfuscate) have finished. This matches SABnzbd's `external_processing`
contract.

## Quick Start

1. Create a script (any language — bash, python, Go binary, etc.)
2. Make it executable: `chmod +x myscript.sh`
3. Place it in the directory configured by `script_dir` in your
   `gonzbd.yaml`
4. Assign it to a category or individual job via the `script` field

```yaml
# gonzbd.yaml
general:
  script_dir: /home/user/.config/gonzbd/scripts

categories:
  - name: tv
    script: my_tv_script.sh
    pp: 3
  - name: movies
    script: organize_movie.py
    pp: 3
```

## Post-Processing Flags (`pp`)

The `pp` field is a **cumulative level** (not a bitmask) that controls which
post-processing stages run before your script is invoked. Each level implies
all lower levels:

| `pp` | Stages enabled |
|------|----------------|
| `0` | Download only — skip all post-processing (script still runs) |
| `1` | + **Repair** — par2 verify and repair |
| `2` | + **Unpack** — extract RAR, 7z, and split archives (implies repair) |
| `3` | + **Delete** — remove par2 and archive files after success (implies unpack + repair) |

Values outside `[0, 3]` are clamped: anything ≥ 3 is treated as 3. Legacy
SABnzbd configs that stored `pp: 7` (a SABnzbd bitmask value) are clamped
to 3 automatically.

Your script receives this value as `SAB_PP` and can use it to adjust behavior
(e.g., skip cleanup logic when `pp < 3` since archives weren't deleted).

## How Scripts Are Invoked

- **No shell**: The script is executed directly (not via `/bin/sh`). Use a
  shebang (`#!/bin/bash`) if you need shell features.
- **Working directory**: Set to the job's final directory (`SAB_COMPLETE_DIR`).
- **Timeout**: 30 seconds by default (configurable). Scripts killed on timeout.
- **Process group**: The script runs in its own process group. On cancellation
  or timeout, the entire group (including child processes) is killed.
- **Output capture**: stdout and stderr are combined and stored in the history
  database (up to 512 KiB). The last non-empty line is shown in the UI.

## Command-Line Arguments

Scripts receive 9 positional arguments (argv[0] is the script itself):

```
script <complete_dir> <nzb_filename> <final_name> "" <category> <group> <pp_status> <failure_url>
```

| Position | Value | Example |
|----------|-------|---------|
| `$0` | Script path | `/scripts/my_script.sh` |
| `$1` | Complete directory (root) | `/downloads/complete` |
| `$2` | Original NZB filename | `My.Show.S01E01.nzb` |
| `$3` | Final job name | `My.Show.S01E01` |
| `$4` | *(empty — historical placeholder)* | `""` |
| `$5` | Category | `tv` |
| `$6` | Usenet group | `alt.binaries.teevee` |
| `$7` | PP status (0=success, 1=repair failed, 2=unpack failed, 3=both failed) | `0` |
| `$8` | Failure URL (if any) | `""` |

> **Note:** Positional args are kept for SABnzbd compatibility. Prefer
> environment variables — they're more complete and less fragile.

## Environment Variables

All variables use the `SAB_` prefix for compatibility with existing SABnzbd
scripts. GoNZBD adds a few extensions (marked below).

### Core Job Info

| Variable | Description | Example |
|----------|-------------|---------|
| `SAB_FILENAME` | Original NZB filename | `My.Show.S01E01.nzb` |
| `SAB_NZB_NAME` | Alias for `SAB_FILENAME` *(Go extension)* | `My.Show.S01E01.nzb` |
| `SAB_FINAL_NAME` | Post-deobfuscation job name | `My.Show.S01E01` |
| `SAB_NZO_ID` | Internal job ID | `SABnzbd_nzo_abc123` |
| `SAB_CAT` | Category | `tv` |
| `SAB_GROUP` | Usenet group | `alt.binaries.teevee` |
| `SAB_STATUS` | Job status (same value as `SAB_PP_STATUS`) | `0` |
| `SAB_PP` | Post-processing level (0–3) | `3` |
| `SAB_SCRIPT` | Script name | `my_script.sh` |
| `SAB_URL` | Source URL (if added via URL) | `https://...` |
| `SAB_PRIORITY` | Job priority | `0` |

### Directories

| Variable | Description | Example |
|----------|-------------|---------|
| `SAB_COMPLETE_DIR` | Root complete directory | `/downloads/complete` |
| `SAB_FINAL_PROCESSING_DIR` | Job's processed directory *(Go extension)* | `/downloads/complete/My.Show.S01E01` |

### Download Stats

| Variable | Description | Example |
|----------|-------------|---------|
| `SAB_BYTES` | Total job size (bytes) | `1073741824` |
| `SAB_BYTES_DOWNLOADED` | Bytes actually downloaded | `1073741824` |
| `SAB_BYTES_TRIED` | Total bytes attempted including retries | `1073741824` |
| `SAB_DOWNLOAD_TIME` | Download duration (seconds) | `120` |
| `SAB_AVG_BPS` | Average download speed (bytes/sec) | `8947848` |
| `SAB_AGE` | Age of NZB in days since posting date | `3` |

### Status & Errors

| Variable | Description | Example |
|----------|-------------|---------|
| `SAB_PP_STATUS` | Post-processing status (0=success, 1=repair failed, 2=unpack failed, 3=both failed) | `0` |
| `SAB_REPAIR` | Whether par2 repair was performed (0=no, 1=yes) | `1` |
| `SAB_UNPACK` | Whether unpacking was performed (0=no, 1=yes) | `1` |
| `SAB_FAIL_MSG` | Failure message (empty on success) | `""` |
| `SAB_FAILURE_URL` | Failure URL (if any) | `""` |
| `SAB_REPORTNAME` | Report name (usually empty) *(Go extension)* | `""` |
| `SAB_ENCRYPTED` | Whether the archive was encrypted (0=unknown, 1=yes) | `0` |
| `SAB_PASSWORD` | Per-job password | `""` |

### Server Info

| Variable | Description | Example |
|----------|-------------|---------|
| `SAB_VERSION` | GoNZBD version | `0.1.0` |
| `SAB_API_KEY` | API key (for callbacks) | `abc123def456` |
| `SAB_API_URL` | API endpoint URL | `http://localhost:4289/api` |

## Return Code

| Exit Code | Meaning |
|-----------|---------|
| `0` | Success — recorded in history |
| Non-zero | Failure — recorded in history with error |

The exit code and captured output are stored in the history database.

## Example Scripts

### Bash: Log completion and notify

```bash
#!/bin/bash
# notify_complete.sh — Log job completion and send a desktop notification.

echo "Job completed: $SAB_FINAL_NAME"
echo "Category: $SAB_CAT"
echo "Size: $SAB_BYTES bytes"
echo "Status: $SAB_PP_STATUS"
echo "Directory: $SAB_FINAL_PROCESSING_DIR"

if [ "$SAB_PP_STATUS" -ne 0 ]; then
    echo "WARNING: Post-processing had errors!"
    exit 1
fi

# Send desktop notification (Linux)
notify-send "Download Complete" "$SAB_FINAL_NAME ($SAB_CAT)"
exit 0
```

### Bash: Move TV shows by category

```bash
#!/bin/bash
# sort_tv.sh — Move completed TV downloads to a Plex-friendly structure.

set -e

SRC="$SAB_FINAL_PROCESSING_DIR"
DEST="/media/plex/TV Shows/$SAB_FINAL_NAME"

if [ "$SAB_PP_STATUS" -ne 0 ]; then
    echo "Skipping sort: post-processing failed"
    exit 1
fi

echo "Moving $SRC → $DEST"
mkdir -p "$DEST"
mv "$SRC"/* "$DEST"/
rmdir "$SRC"

echo "Done: $DEST"
exit 0
```

### Python: Call Sonarr/Radarr API

```python
#!/usr/bin/env python3
"""notify_sonarr.py — Tell Sonarr to rescan after download."""

import os
import sys
import json
import urllib.request

SONARR_URL = "http://localhost:8989"
SONARR_API_KEY = "your-sonarr-api-key"

status = int(os.environ.get("SAB_PP_STATUS", "1"))
name = os.environ.get("SAB_FINAL_NAME", "unknown")
directory = os.environ.get("SAB_FINAL_PROCESSING_DIR", "")

print(f"Job: {name}")
print(f"Status: {status}")
print(f"Directory: {directory}")

if status != 0:
    print("Post-processing failed, skipping Sonarr notification")
    sys.exit(1)

# Trigger Sonarr rescan
req = urllib.request.Request(
    f"{SONARR_URL}/api/v3/command",
    data=json.dumps({"name": "DownloadedEpisodesScan", "path": directory}).encode(),
    headers={
        "Content-Type": "application/json",
        "X-Api-Key": SONARR_API_KEY,
    },
    method="POST",
)
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        print(f"Sonarr responded: {resp.status}")
except Exception as e:
    print(f"Sonarr notification failed: {e}")
    sys.exit(1)

sys.exit(0)
```

## Disabling Scripts

Set the script field to `"None"` (case-insensitive) or leave it empty to
skip script execution for a category or job:

```yaml
categories:
  - name: default
    script: "None"  # No script
```

## Testing Scripts

### Manual Testing

Test your script by setting the environment variables and running it directly:

```bash
# Set up a test environment
export SAB_FINAL_NAME="Test.Show.S01E01"
export SAB_CAT="tv"
export SAB_PP_STATUS="0"
export SAB_COMPLETE_DIR="/tmp/test_complete"
export SAB_FINAL_PROCESSING_DIR="/tmp/test_complete/Test.Show.S01E01"
export SAB_FILENAME="Test.Show.S01E01.nzb"
export SAB_NZO_ID="test_001"
export SAB_BYTES="1073741824"
export SAB_BYTES_DOWNLOADED="1073741824"
export SAB_DOWNLOAD_TIME="60"
export SAB_AVG_BPS="17895697"
export SAB_VERSION="0.1.0"
export SAB_API_KEY="test_key"
export SAB_API_URL="http://localhost:4289/api"
export SAB_SCRIPT="my_script.sh"
export SAB_PP="3"
export SAB_GROUP=""
export SAB_URL=""
export SAB_FAIL_MSG=""
export SAB_FAILURE_URL=""
export SAB_REPORTNAME=""
export SAB_STATUS="0"
export SAB_NZB_NAME="Test.Show.S01E01.nzb"
export SAB_BYTES_TRIED="1073741824"
export SAB_PASSWORD=""
export SAB_ENCRYPTED="0"
export SAB_PRIORITY="0"
export SAB_REPAIR="1"
export SAB_UNPACK="1"
export SAB_AGE="3"
export SAB_PAR2_COMMAND=""
export SAB_RAR_COMMAND=""
export SAB_7ZIP_COMMAND=""

# Create the test directory structure
mkdir -p "$SAB_FINAL_PROCESSING_DIR"
echo "test content" > "$SAB_FINAL_PROCESSING_DIR/test_file.mkv"

# Run your script
./scripts/my_script.sh "$SAB_COMPLETE_DIR" "$SAB_FILENAME" \
    "$SAB_FINAL_NAME" "" "$SAB_CAT" "$SAB_GROUP" "0" ""

echo "Exit code: $?"
```

### Integration Testing with GoNZBD

Use the `RunScript` function from the `postproc` package in Go tests:

```go
package mytest

import (
    "context"
    "os"
    "path/filepath"
    "testing"

    "github.com/hobeone/gonzbd/internal/postproc"
)

func TestMyScript(t *testing.T) {
    // Create a temp directory to simulate the job output
    dir := t.TempDir()
    os.WriteFile(filepath.Join(dir, "test.mkv"), []byte("fake video"), 0o644)

    scriptPath := filepath.Join("path", "to", "my_script.sh")

    in := postproc.ScriptInput{
        FinalDir:    dir,
        CompleteDir: filepath.Dir(dir),
        NZBName:     "Test.Show.S01E01.nzb",
        JobName:     "Test.Show.S01E01",
        Category:    "tv",
        Status:      0,
        PPFlags:     3,
        ScriptName:  "my_script.sh",
        NZOID:       "test_001",
        Version:     "0.1.0",
        Bytes:       1073741824,
    }

    result := postproc.RunScript(t.Context(), scriptPath, in)

    if result.Err != nil {
        t.Fatalf("script failed: %v\nOutput:\n%s", result.Err, result.LogBody)
    }
    t.Logf("Exit code: %d", result.ExitCode)
    t.Logf("Output:\n%s", result.LogBody)
    t.Logf("Duration: %v", result.Duration)
}
```

### Testing Tips

- **Test both success and failure**: Set `SAB_PP_STATUS=1` to simulate a
  failed post-processing run and verify your script handles it correctly.
- **Test with special characters**: Use job names containing spaces, quotes,
  and unicode to ensure your script handles them.
- **Check exit codes**: GoNZBD treats any non-zero exit as a failure. Use
  `set -e` in bash scripts to catch unexpected errors.
- **Output is captured**: Write diagnostic info to stdout/stderr — it's
  stored in the history database and visible in the UI.
- **Timeout**: Scripts that run longer than 30 seconds are killed. For long
  operations, consider backgrounding the work and exiting 0 immediately.

## SABnzbd Compatibility

GoNZBD's script interface is designed to be compatible with existing SABnzbd
post-processing scripts. Key differences:

| Feature | SABnzbd (Python) | GoNZBD |
|---------|------------------|--------|
| `SAB_FINAL_PROCESSING_DIR` | Not emitted | ✅ Emitted (Go extension) |
| `SAB_NZB_NAME` | Not emitted | ✅ Alias for `SAB_FILENAME` |
| `SAB_PYTHONUNBUFFERED` | Set for `.py` scripts | Not set |
| `SAB_PROGRAM_DIR` | Set to SABnzbd install dir | Not set |
| `SAB_PAR2_COMMAND` | Set | ✅ Set (empty string if not configured) |
| `SAB_RAR_COMMAND` | Set | ✅ Set (empty string if not configured) |
| `SAB_7ZIP_COMMAND` | Set | ✅ Set (empty string if not configured) |
| Script timeout | None (runs forever) | 30 seconds (configurable) |
| Process group kill | No | Yes — kills entire group |

Most SABnzbd scripts will work unchanged. Scripts that depend on
`SAB_PROGRAM_DIR` will need minor adjustments.
