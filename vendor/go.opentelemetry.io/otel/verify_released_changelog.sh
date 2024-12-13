#!/bin/bash

# Copyright The OpenTelemetry Authors
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

TARGET="${1:?Must provide target ref}"

FILE="CHANGELOG.md"
TEMP_DIR=$(mktemp -d)
echo "Temp folder: $TEMP_DIR"

# Only the latest commit of the feature branch is available
# automatically. To diff with the base branch, we need to
# fetch that too (and we only need its latest commit).
git fetch origin "${TARGET}" --depth=1

# Checkout the previous version on the base branch of the changelog to tmpfolder
git --work-tree="$TEMP_DIR" checkout FETCH_HEAD $FILE

PREVIOUS_FILE="$TEMP_DIR/$FILE"
CURRENT_FILE="$FILE"
PREVIOUS_LOCKED_FILE="$TEMP_DIR/previous_locked_section.md"
CURRENT_LOCKED_FILE="$TEMP_DIR/current_locked_section.md"

# Extract released sections from the previous version
awk '/^<!-- Released section -->/ {flag=1} /^<!-- Released section ended -->/ {flag=0} flag' "$PREVIOUS_FILE" > "$PREVIOUS_LOCKED_FILE"

# Extract released sections from the current version
awk '/^<!-- Released section -->/ {flag=1} /^<!-- Released section ended -->/ {flag=0} flag' "$CURRENT_FILE" > "$CURRENT_LOCKED_FILE"

# Compare the released sections
if ! diff -q "$PREVIOUS_LOCKED_FILE" "$CURRENT_LOCKED_FILE"; then
    echo "Error: The released sections of the changelog file have been modified."
    diff "$PREVIOUS_LOCKED_FILE" "$CURRENT_LOCKED_FILE"
    rm -rf "$TEMP_DIR"
    false
fi

rm -rf "$TEMP_DIR"
echo "The released sections remain unchanged."
