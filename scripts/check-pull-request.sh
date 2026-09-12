#!/bin/sh
# Checks the pull request rules from CLAUDE.md.
# The argument is the path of the GitHub event payload.
set -eu

event="${1:-$GITHUB_EVENT_PATH}"
status=0

body=$(jq -r '.pull_request.body // ""' "$event")
if printf '%s' "$body" | grep -qiE '(close[sd]?|fixe?[sd]?|resolve[sd]?) #[0-9]+'; then
	echo "linked issue: ok"
else
	echo "error: the description has no line that closes an issue"
	echo "       write 'Closes #<number>' in the description"
	status=1
fi

types=$(jq -r '.pull_request.labels[].name' "$event" | grep -c '^type:' || true)
if [ "$types" -eq 1 ]; then
	echo "type label: ok"
else
	echo "error: the pull request has $types type labels, and it needs 1"
	echo "       use the type label of the issue"
	status=1
fi

exit "$status"
