#!/bin/sh
# Checks the pull request rules from CLAUDE.md.
# The argument is the path of the GitHub event payload.
set -eu

event="${1:-$GITHUB_EVENT_PATH}"

body=$(jq -r '.pull_request.body // ""' "$event")
repo=$(jq -r '.repository.full_name' "$event")

issue=$(printf '%s' "$body" |
	grep -oiE '(close[sd]?|fixe?[sd]?|resolve[sd]?) #[0-9]+' |
	head -1 | tr -dc '0-9')

if [ -z "$issue" ]; then
	echo "error: the description has no line that closes an issue"
	echo "       write 'Closes #<number>' in the description"
	exit 1
fi
echo "linked issue: #$issue"

if ! labels=$(gh issue view "$issue" --repo "$repo" --json labels \
	--jq '.labels[].name'); then
	echo "error: the check cannot read issue #$issue"
	exit 1
fi

types=$(printf '%s\n' "$labels" | grep -c '^type:' || true)
if [ "$types" -ne 1 ]; then
	echo "error: issue #$issue has $types type labels, and it needs 1"
	echo "       put one type label on the issue"
	exit 1
fi
echo "type label: ok"
