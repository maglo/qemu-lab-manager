# Changelog fragments

One pull request writes one file here. The file says what changed, and the
release compiles every file into `CHANGELOG.md`. See
[`docs/design/changelog.md`](../../docs/design/changelog.md) for the reason.

Name the file `<issue>-<slug>.yaml`, such as `42-serial-reconnect.yaml`.

```yaml
---
bugfixes:
  - "serial - the broker drops a subscriber that does not read
    (https://github.com/maglo/qemu-lab-manager/issues/42)."
```

| Section | Use |
|---|---|
| `release_summary` | One paragraph about the release. One per release. |
| `breaking_changes` | The operator must change something to upgrade. |
| `major_changes` | New behavior that changes how the lab is used. |
| `minor_changes` | New behavior, a new setting, a new endpoint. |
| `deprecated_features` | Behavior that a later release removes. |
| `removed_features` | Behavior that this release removes. |
| `security_fixes` | A fix for a hole. |
| `bugfixes` | The behavior was wrong, and now it is right. |
| `known_issues` | A fault that this release keeps. |
| `trivial` | CI, tools and documents. The release drops it. |

Rules:

- One file holds several sections.
- Write each entry in Simplified Technical English, as `CLAUDE.md` says.
- Start the entry with the component, such as `inventory` or `api`.
- End the entry with a period.
- Link the issue at the end of the entry.
- Run `./scripts/check-changelog.sh` before you push.
