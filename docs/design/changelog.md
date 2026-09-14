# The changelog

## 1. The problem

A reader of the repository cannot see what changed between two versions, and
an operator who upgrades labview cannot see what the upgrade brings. The
commit log is not that document. It names each step of the work, in the words
of the person who did the work, and it holds the steps that a reader never
sees, such as a fix for a check.

A file that every pull request edits is also a file that every pull request
conflicts on. Two branches that both add a line to the top of `CHANGELOG.md`
conflict, and the conflict appears at merge time, after the review.

## 2. The decision

A pull request writes one fragment file. A release compiles the fragments.

This is the model of `maglo/ansible-collection-qemu`, which uses
`antsibull-changelog`. That tool is part of the Ansible ecosystem and it reads
an Ansible collection, so this repository carries its own tool,
`scripts/changelog.py`. The format of the fragment and the names of the
sections are the same, so a person who works in both repositories learns one
format.

Two branches write two files with two names, so they do not conflict.

## 3. The files

| Path | What it is |
|---|---|
| `changelogs/fragments/` | One file for each pull request. |
| `changelogs/changelog.yaml` | The released entries, as data. |
| `CHANGELOG.md` | The document for a reader. |
| `scripts/changelog.py` | The tool. |
| `scripts/check-changelog.sh` | The check. |

`changelogs/changelog.yaml` is the source, and `CHANGELOG.md` is the output.
The tool writes the whole document from the data at each release, so it never
edits markdown in place and a release cannot corrupt the entries of an older
release. The lint renders the document again and compares it, so a hand edit
of `CHANGELOG.md` fails the check.

## 4. The sections

`release_summary`, `breaking_changes`, `major_changes`, `minor_changes`,
`deprecated_features`, `removed_features`, `security_fixes`, `bugfixes` and
`known_issues` reach `CHANGELOG.md`, in that order.

`trivial` does not. A change to a check, to a script or to a document is work
that the reviewer sees and that the operator of a lab never sees. The fragment
still records it, because the alternative is a pull request with no fragment,
and then the check cannot tell a trivial change from a forgotten one.

## 5. The check

`checks.yml` runs `scripts/check-changelog.sh` in the `changelog` job. It
reads each fragment, and it fails on an unknown section, on an entry that is
not text, on an entry that does not end with a period, and on a `CHANGELOG.md`
that does not match the data.

The `pull request rules` job asks the pull request for a fragment. A pull
request that changes `CHANGELOG.md` is the release, and it adds no fragment,
so the check lets it pass.

## 6. The line length

`check-docs.sh` holds a markdown line to 80 characters, and it allows a longer
line that carries a URL, because a URL does not break. The tool wraps each
entry to 80 characters and it never breaks a word, so an entry that ends with
an issue URL produces one long line that the document check allows.

## 7. The release

The owner cuts a release in a pull request, and the merge is the release
point.

    ./scripts/changelog.py release --version 0.1.0

The command reads the fragments, drops the `trivial` entries, writes the
version into `changelogs/changelog.yaml` with the date of today, writes
`CHANGELOG.md` again and deletes the fragments. It refuses a version that
`changelogs/changelog.yaml` already holds.

The owner reviews the diff, merges the pull request and tags the merge commit
`v0.1.0`.

## 8. The tag

`release.yml` runs on a `v*` tag.

It asks `scripts/changelog.py notes` for the notes of that version, builds
`labview` for Linux and macOS on both architectures, and creates a GitHub
release with the notes and the binaries.

The notes come first. A tag that runs ahead of the release pull request names
a version that `changelogs/changelog.yaml` does not hold, and the job fails
there, before it creates a release that has no notes.

`ci.yml` also runs on the tag, so the commit that the tag names passes the
same checks as a pull request. The version reaches the binary through
`-X main.version`, so `labview -version` prints `0.1.0` from a binary of the
0.1.0 release.

## 9. What is not built

- **A tag from CI.** The owner tags. A workflow that tags on a merge would
  release whatever reached `main`, and the release pull request is the point
  where a human reads the notes.
- **A version file.** The tag carries the version, and the build stamps it.
  There is no second place that states a version and drifts from the tag.
- **A pre-release version.** `scripts/changelog.py` takes `X.Y.Z` only. A
  release candidate needs a rule for the order of `0.2.0-rc1` and `0.2.0`,
  and the lab has no use for one yet.
