# CLAUDE.md

Rules for agents and humans who work in this repository.

## Repository

qemu-lab-manager holds the tooling for the QEMU lab. It builds one Go binary,
`labview`.

| Path | Content | Its document |
|---|---|---|
| `cmd/labview` | The main package. | [`labview.md`](docs/design/labview.md) |
| `internal/` | The packages of the service. | [`labview.md`](docs/design/labview.md) |
| `internal/web/static` | The browser application. | [`labview.md`](docs/design/labview.md) |
| `docs/design/` | One design document for each component. | |
| `changelogs/` | The fragments and the released entries. | [`changelog.md`](docs/design/changelog.md) |
| `test/lab` | The fake lab and the browser checks. | [`test/lab/README.md`](test/lab/README.md) |
| `deploy/` | The systemd unit, the polkit rule and the nginx file. | [`labview.md`](docs/design/labview.md) |
| `scripts/` | The check scripts. | [`checks.md`](docs/design/checks.md) |
| `.github/workflows/` | The workflows. | [`checks.md`](docs/design/checks.md) |
| `Dockerfile` | The container image. | [`container.md`](docs/design/container.md) |

A design document gives the reason for a decision. `README.md` gives the build
command and the run command.

This file holds the rules. It does not describe what a part does, because the
document of that part does. Follow the link.

## Workflow

Every change follows these steps.

1. Find an issue, or file one. Every pull request has an issue.
2. Make a branch from `main`.
3. Make the change. Update the documents in the same branch.
4. Write a changelog fragment in `changelogs/fragments/`.
5. Open a pull request. Write `Closes #<number>` in the description.
6. Make the checks pass.
7. The owner reviews the pull request.
8. The owner merges the pull request.

## Limits for the agent

- Do not merge a pull request. Only a human merges.
- Do not push to `main`. Push to a branch and open a pull request.
- Do not force-push a branch that another person uses.
- Do not close an issue by hand. The merge closes it through the link.
- File the issue before you open the pull request.
- Ask the user before you do work that the issue does not name.
- Do not edit `CHANGELOG.md` or `changelogs/changelog.yaml`. The release
  writes both files.

## Review and merge

The owner reviews and merges. The owner can also enable auto-merge.

The repository does not need an approving review. The agent opens a pull
request with the token of the owner. GitHub does not let the author approve
the pull request, so a rule that needs an approval blocks every pull request
of the agent.

The checks are the gate for a merge. The branch protection rules of `main`
need 0 approvals, and they need `checks passed` and `ci passed`.

## Issues and labels

The tracking is simple. Each issue has one type label. The other labels are
optional.

Type, one for each issue:

| Label | Use |
|---|---|
| `type:bug` | The behavior is wrong. |
| `type:feature` | New behavior. |
| `type:docs` | Documents only. |
| `type:chore` | Build, tools, dependencies, repository setup. |

Status, optional:

| Label | Use |
|---|---|
| `status:blocked` | The work cannot continue. |
| `status:needs-decision` | A human decides first. |

Priority, optional:

| Label | Use |
|---|---|
| `prio:high` | Do this first. |
| `prio:low` | Do this last. |

An issue without a priority label has normal priority.

A label belongs on the issue. A pull request does not get a label, because the
issue carries the type.

A human creates these labels in the repository settings.

## Tests

Run these commands before you push.

```sh
go test -race ./...           # the Go tests
test/lab/run.sh               # labview in a browser, against a fake lab
./scripts/check-docs.sh       # the markdown rules
./scripts/check-workflows.sh  # the gate of each workflow
./scripts/check-changelog.sh  # the changelog fragments
```

A change of behavior comes with a test.
[`test/lab/README.md`](test/lab/README.md) says how to set up the browser
layer, how to add a check, and why the layer exists.

## Checks

GitHub Actions runs the checks for each pull request.
[`docs/design/checks.md`](docs/design/checks.md) says what each workflow
holds, what each job checks, and how the gate works.

Two rules live here, because they bind every change.

- A new job goes in the `needs` list of the gate of its workflow. The
  `workflows` check fails if you forget.
- Do not change a check to make a red check pass.

## Changelog

Every pull request writes one file in `changelogs/fragments/`. Name the file
`<issue>-<slug>.yaml`.

```yaml
---
bugfixes:
  - "serial - the broker drops a subscriber that does not read
    (https://github.com/maglo/qemu-lab-manager/issues/42)."
```

`changelogs/fragments/README.md` lists each section. Use `trivial` for a
change that the operator of a lab never sees, such as a check, a script or a
document. A `trivial` entry does not reach `CHANGELOG.md`.

Start the entry with the component. End the entry with a period. Put the URL
of the issue at the end.

The owner cuts a release with `./scripts/changelog.py release --version X.Y.Z`
and then tags the merge commit `vX.Y.Z`. `docs/design/changelog.md` gives the
whole process.

## Documents

A pull request updates the document of the part it changes. The two go in the
same pull request.

The table in the Repository section names the document of each part. Add a
browser check, and `test/lab/README.md` changes. Add a CI job, and
`docs/design/checks.md` changes. Change how a lease expires, and
`docs/design/labview.md` changes.

- A change that makes a document wrong also corrects that document.
- A design document shows the current design. It is not a history.
- Do not add a changelog section to a design document.
- A new document goes in the table, so a reader reaches it from this file.

## Comments in code

- Write few comments.
- Write a comment only for a reason that the code cannot show.
- Do not repeat in words what the code does.
- Do not write about the old behavior.
- Delete a comment when the comment becomes wrong.

## Language

Use Simplified Technical English in issues, commits, comments, pull requests
and documents.

- Use short sentences. Use 20 words or fewer in each sentence.
- Give one instruction in one sentence.
- Use the active voice.
- Use the present tense.
- Use one word for one thing. Do not change the word for variety.
- Use simple words. Write `use`, not `utilize`.
- Write what the software does now.
- Do not describe the old behavior. Write `The broker keeps one connection`,
  not `The broker kept many connections, and now it keeps one`.
- Do not use sales words such as `seamless` or `powerful`.

## Commits

- Put one logical change in one commit.
- Write the subject in the imperative. Use 50 characters or fewer.
- Write why you make the change in the body. Break the lines at 72 characters.
- Name the old behavior only when the reader needs it to understand the fix.
