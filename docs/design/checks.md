# The checks

## 1. What this is

GitHub Actions runs every check for each pull request. This document says
what each workflow holds and why. `CLAUDE.md` holds the rules of the
repository and points here; it does not repeat this list.

## 2. `checks.yml`

The checks of the repository itself. It runs on a pull request and on a push
to `main`.

| Job | What it checks |
|---|---|
| `documents` | Each markdown file. |
| `workflows` | The gate job of each workflow. |
| `changelog` | The fragments, and `CHANGELOG.md` against the data. |
| `pull request rules` | The description and the issue. |

`documents` runs `scripts/check-docs.sh`. A line has 80 characters or fewer.
A table line and a line with a URL can be longer, because neither breaks. A
line does not end with a space. A file ends with a newline.

`workflows` runs `scripts/check-workflows.sh`. The gate job of a workflow
depends on every other job of that workflow.

`changelog` runs `scripts/check-changelog.sh`. It reads each fragment in
`changelogs/fragments/`, and it renders `CHANGELOG.md` again from
`changelogs/changelog.yaml` and compares the two.
[`changelog.md`](changelog.md) gives the format.

`pull request rules` runs `scripts/check-pull-request.sh`. The description
closes an issue, that issue has one type label, and the pull request adds a
changelog fragment. A release pull request changes `CHANGELOG.md` and adds no
fragment, so the check lets it pass.

This job needs the GitHub event payload, so it runs in CI only. The other
three run on a laptop.

## 3. `ci.yml`

The Go code and the image. It runs on a pull request, on a push to `main` and
on a `v*` tag.

| Job | What it does |
|---|---|
| `build, vet and test` | `gofmt`, `go vet`, the build and the tests. |
| `browser checks` | labview in a browser, against a fake lab. |
| `container image` | The image, built always and pushed on a push. |

The tests run with the race detector. The broker fans one upstream connection
out to many subscribers under one lock, so a regression there is a data race
long before it is a visible bug.

`browser checks` keeps the screenshots as an artifact. A layout bug is
quicker to read from an image than from an assertion message.
[`test/lab/README.md`](../../test/lab/README.md) says why the browser layer
exists and how to add a check.

A pull request builds the image and pushes nothing, so a broken `Dockerfile`
fails the check before `main` gets an image from it.
[`container.md`](container.md) gives the design of the image.

## 4. `release.yml`

A `v*` tag.

| Job | What it does |
|---|---|
| `binaries and notes` | The notes of the tag, the binaries, the release. |

It reads the notes of that version first. A tag that runs ahead of the
release pull request names a version that `changelogs/changelog.yaml` does
not hold, and the job fails there, before it creates a release with no notes.

`ci.yml` runs on the tag as well, so the image carries the version.
[`changelog.md`](changelog.md) gives the release process.

## 5. The gate

Each workflow ends with one gate job: `checks passed`, `ci passed` and
`release passed`. A gate depends on every other job of its workflow, and it
fails if one of them does not pass. A skipped job and a cancelled job also
fail the gate.

The ruleset of `main` names `checks passed` and `ci passed`. It names no
other job, and it does not name `release passed`, because a tag is not a
pull request.

A new job therefore needs no change in the ruleset. Add the new job to the
`needs` list of the gate of its workflow. `workflows` fails if you forget.
