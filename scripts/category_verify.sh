#!/bin/bash
# category_verify.sh — Example post-processing script for gonzbd
#
# Place this script in your script_dir and set a category's "script" field
# to "category_verify.sh". When gonzbd finishes downloading a job in that
# category, it will run this script with the SAB_* environment variables
# and positional arguments described below.
#
# This script logs every environment variable and argument it receives
# to a file, making it easy to verify that per-category defaults (PP,
# Script, Priority) are being resolved correctly.
#
# Setup:
#   1. Set general.script_dir in gonzbd.yaml to a directory, e.g.:
#        script_dir: /home/user/gonzbd-scripts
#
#   2. Copy this script there and make it executable:
#        cp scripts/category_verify.sh /home/user/gonzbd-scripts/
#        chmod +x /home/user/gonzbd-scripts/category_verify.sh
#
#   3. Set a category's script field to this script's name:
#        categories:
#          - name: tv
#            pp: 3
#            script: category_verify.sh   # <-- this name
#            priority: 1
#            dir: TV
#
#   4. Add an NZB with cat=tv (no explicit pp/script/priority).
#      After post-processing, check the log file written below.
#
# Positional arguments (matches Python SABnzbd's external_processing):
#   $1 = complete_dir    (SAB_COMPLETE_DIR)
#   $2 = nzb_filename    (SAB_FILENAME)
#   $3 = job_name        (SAB_FINAL_NAME)
#   $4 = report_name     (SAB_REPORTNAME — always empty)
#   $5 = category        (SAB_CAT)
#   $6 = group           (SAB_GROUP)
#   $7 = status          (SAB_PP_STATUS — 0=OK, 1=failed)
#   $8 = failure_url     (SAB_FAILURE_URL)
#
# SAB_* environment variables are also set (see script.go for the full list).
#
# Exit code:
#   0 = success (gonzbd logs "script completed successfully")
#   non-zero = failure (gonzbd marks job with script error)
#
# The last line of stdout is captured as the script result in history.

set -euo pipefail

# --- Configuration ---
# Where to write the verification log. Override via VERIFY_LOG env var.
LOG_DIR="${SAB_FINAL_PROCESSING_DIR:-${1:-.}}"
LOG_FILE="${VERIFY_LOG:-${LOG_DIR}/category_verify.log}"

# --- Write Header ---
{
    echo "=============================================="
    echo "gonzbd Post-Processing Script Verification"
    echo "=============================================="
    echo "Timestamp: $(date -Iseconds)"
    echo ""

    # --- Positional Arguments ---
    echo "--- Positional Arguments ---"
    echo "  \$1 complete_dir:  ${1:-<not set>}"
    echo "  \$2 nzb_filename:  ${2:-<not set>}"
    echo "  \$3 job_name:      ${3:-<not set>}"
    echo "  \$4 report_name:   ${4:-<not set>}"
    echo "  \$5 category:      ${5:-<not set>}"
    echo "  \$6 group:         ${6:-<not set>}"
    echo "  \$7 status:        ${7:-<not set>}"
    echo "  \$8 failure_url:   ${8:-<not set>}"
    echo ""

    # --- Key SAB_* Environment Variables ---
    echo "--- SAB Environment Variables ---"
    echo "  SAB_CAT:                   ${SAB_CAT:-<not set>}"
    echo "  SAB_PP:                    ${SAB_PP:-<not set>}"
    echo "  SAB_SCRIPT:                ${SAB_SCRIPT:-<not set>}"
    echo "  SAB_FINAL_NAME:            ${SAB_FINAL_NAME:-<not set>}"
    echo "  SAB_FILENAME:              ${SAB_FILENAME:-<not set>}"
    echo "  SAB_NZO_ID:                ${SAB_NZO_ID:-<not set>}"
    echo "  SAB_STATUS:                ${SAB_STATUS:-<not set>}"
    echo "  SAB_PP_STATUS:             ${SAB_PP_STATUS:-<not set>}"
    echo "  SAB_BYTES:                 ${SAB_BYTES:-<not set>}"
    echo "  SAB_BYTES_DOWNLOADED:      ${SAB_BYTES_DOWNLOADED:-<not set>}"
    echo "  SAB_COMPLETE_DIR:          ${SAB_COMPLETE_DIR:-<not set>}"
    echo "  SAB_FINAL_PROCESSING_DIR:  ${SAB_FINAL_PROCESSING_DIR:-<not set>}"
    echo "  SAB_URL:                   ${SAB_URL:-<not set>}"
    echo "  SAB_GROUP:                 ${SAB_GROUP:-<not set>}"
    echo "  SAB_DOWNLOAD_TIME:         ${SAB_DOWNLOAD_TIME:-<not set>}"
    echo "  SAB_AVG_BPS:               ${SAB_AVG_BPS:-<not set>}"
    echo "  SAB_VERSION:               ${SAB_VERSION:-<not set>}"
    echo "  SAB_FAIL_MSG:              ${SAB_FAIL_MSG:-<not set>}"
    echo ""

    # --- Directory Contents ---
    echo "--- Files in Processing Dir ---"
    if [ -d "$LOG_DIR" ]; then
        ls -lhA "$LOG_DIR" 2>/dev/null || echo "  (empty or not readable)"
    else
        echo "  (directory does not exist)"
    fi
    echo ""
    echo "=============================================="
} | tee "$LOG_FILE"

# The last line of stdout is shown in gonzbd's history UI.
echo "category_verify: OK — cat=${SAB_CAT:-?}, pp=${SAB_PP:-?}, script=${SAB_SCRIPT:-?}"
