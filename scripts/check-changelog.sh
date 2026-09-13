#!/bin/sh
# Checks the changelog rules from CLAUDE.md.
set -eu

cd "$(dirname "$0")/.."
exec python3 scripts/changelog.py lint
