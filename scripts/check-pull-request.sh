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

# A release pull request compiles the fragments away, so it adds none.
if ! files=$(gh pr view "$(jq -r '.pull_request.number' "$event")" \
	--repo "$repo" --json files --jq '.files[].path'); then
	echo "error: the check cannot read the files of the pull request"
	exit 1
fi

if printf '%s\n' "$files" | grep -q '^CHANGELOG\.md$'; then
	echo "changelog fragment: not needed, this is a release"
	exit 0
fi

if ! printf '%s\n' "$files" | grep -q '^changelogs/fragments/.*\.ya\?ml$'; then
	echo "error: the pull request adds no changelog fragment"
	echo "       write one file in changelogs/fragments/"
	echo "       changelogs/fragments/README.md gives the format"
	exit 1
fi
echo "changelog fragment: ok"
