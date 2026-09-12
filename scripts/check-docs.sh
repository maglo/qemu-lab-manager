#!/bin/sh
# Checks the document rules from CLAUDE.md.
set -eu

cd "$(dirname "$0")/.."
status=0

for file in $(git ls-files '*.md'); do
	awk -v file="$file" '
		/[ \t]+$/ { printf "%s:%d: the line ends with a space\n", file, FNR; bad = 1 }
		/\t/      { printf "%s:%d: the line has a tab\n", file, FNR; bad = 1 }
		/^```/    { fence = !fence; next }
		fence     { next }
		/^ *\|/   { next }
		length($0) > 80 && $0 !~ /:\/\// {
			printf "%s:%d: the line is longer than 80 characters\n", file, FNR
			bad = 1
		}
		END { exit bad ? 1 : 0 }
	' "$file" || status=1

	if [ -n "$(tail -c 1 "$file")" ]; then
		echo "$file: the file does not end with a newline"
		status=1
	fi
done

if [ "$status" -eq 0 ]; then
	echo "documents: ok"
fi

exit "$status"
