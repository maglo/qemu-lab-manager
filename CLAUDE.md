# CLAUDE.md

Rules for agents and humans who work in this repository.

## Repository

qemu-lab-manager holds the tooling for the QEMU lab. `docs/design/` holds one
design document for each component. The repository is design only today. Code
comes later.

## Workflow

Every change follows these steps.

1. Find an issue, or file one. Every pull request has an issue.
2. Make a branch from `main`.
3. Make the change. Update the documents in the same branch.
4. Open a pull request. Write `Closes #<number>` in the description.
5. A human reviews the pull request.
6. A human merges the pull request.

## Limits for the agent

- Do not merge a pull request. Only a human merges.
- Do not push to `main`. Push to a branch and open a pull request.
- Do not force-push a branch that another person uses.
- Do not close an issue by hand. The merge closes it through the link.
- File the issue before you open the pull request.
- Ask the user before you do work that the issue does not name.

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

An issue without a priority label has normal priority. A pull request gets the
same type label as its issue.

A human creates these labels in the repository settings.

## Documents

- Documents and code change together, in the same pull request.
- A change that makes a document wrong also corrects that document.
- A design document shows the current design. It is not a history.
- Do not add a changelog section to a design document.

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
