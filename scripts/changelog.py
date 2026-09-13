#!/usr/bin/env python3
"""The changelog tool. See docs/design/changelog.md.

A pull request writes a fragment. A release compiles the fragments into
changelogs/changelog.yaml and writes CHANGELOG.md from it.
"""

import argparse
import datetime
import pathlib
import re
import sys
import textwrap

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent
FRAGMENTS = ROOT / "changelogs" / "fragments"
CHANGES = ROOT / "changelogs" / "changelog.yaml"
DOCUMENT = ROOT / "CHANGELOG.md"

# The order here is the order of the sections in CHANGELOG.md.
SECTIONS = [
    ("release_summary", "Release Summary"),
    ("breaking_changes", "Breaking Changes"),
    ("major_changes", "Major Changes"),
    ("minor_changes", "Minor Changes"),
    ("deprecated_features", "Deprecated Features"),
    ("removed_features", "Removed Features"),
    ("security_fixes", "Security Fixes"),
    ("bugfixes", "Bugfixes"),
    ("known_issues", "Known Issues"),
]
TITLES = dict(SECTIONS)

# A trivial change is invisible to a user of labview. The fragment records it
# for the reviewer, and the release drops it.
TRIVIAL = "trivial"

VERSION = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")

# check-docs.sh allows a long line that holds a URL, because a URL does not
# break. Every other line of CHANGELOG.md wraps here.
WIDTH = 80

HEADER = """# Changelog

Every release of labview, newest first. A pull request does not edit this
file. It adds a fragment to `changelogs/fragments/`, and a release compiles
the fragments. See `docs/design/changelog.md`.
"""


def rel(path):
    return path.relative_to(ROOT)


def fragment_paths():
    if not FRAGMENTS.is_dir():
        return []
    paths = [p for p in FRAGMENTS.iterdir() if p.suffix in (".yaml", ".yml")]
    return sorted(paths)


def flatten(value):
    """Reads a section value as a list of entries."""
    if isinstance(value, str):
        return [value]
    if isinstance(value, list):
        return value
    return None


def normalize(entry):
    """Joins a folded YAML string into one line."""
    return " ".join(entry.split())


def lint_fragment(path, report):
    name = rel(path)
    text = path.read_text(encoding="utf-8")
    try:
        content = yaml.safe_load(text)
    except yaml.YAMLError as err:
        report(f"{name}: the file is not YAML: {err}")
        return None

    if content is None:
        report(f"{name}: the file is empty")
        return None
    if not isinstance(content, dict):
        report(f"{name}: the file is not a mapping of sections")
        return None

    known = set(TITLES) | {TRIVIAL}
    clean = {}
    for section, value in content.items():
        if section not in known:
            names = ", ".join(sorted(known))
            report(f"{name}: '{section}' is not a section. Use one of: {names}")
            continue

        entries = flatten(value)
        if entries is None:
            report(f"{name}: '{section}' is not a string and not a list")
            continue
        if not entries:
            report(f"{name}: '{section}' is empty")
            continue
        if section == "release_summary" and len(entries) != 1:
            report(f"{name}: '{section}' holds {len(entries)} entries, and it "
                   "takes 1")
            continue

        good = []
        for entry in entries:
            if not isinstance(entry, str) or not entry.strip():
                report(f"{name}: '{section}' holds an entry that is not text")
                continue
            entry = normalize(entry)
            if not entry.endswith("."):
                start = entry if len(entry) <= 60 else entry[:57] + "..."
                report(f"{name}: '{section}': the entry does not end with a "
                       f"period: {start}")
                continue
            good.append(entry)
        if good:
            clean[section] = good

    return clean


def load_releases():
    if not CHANGES.is_file():
        return {}
    content = yaml.safe_load(CHANGES.read_text(encoding="utf-8")) or {}
    return content.get("releases") or {}


def order(releases):
    """Newest release first."""
    return sorted(releases, key=lambda v: [int(n) for n in v.split(".")],
                  reverse=True)


def render(releases):
    lines = [HEADER]
    for version in order(releases):
        release = releases[version]
        lines.append(f"## {version} -- {release['release_date']}\n")
        changes = release.get("changes") or {}
        for section, title in SECTIONS:
            entries = changes.get(section)
            if not entries:
                continue
            lines.append(f"### {title}\n")
            if section == "release_summary":
                lines.append(textwrap.fill(
                    entries[0], width=WIDTH,
                    break_long_words=False, break_on_hyphens=False) + "\n")
                continue
            block = [textwrap.fill(
                entry, width=WIDTH, initial_indent="- ",
                subsequent_indent="  ",
                break_long_words=False, break_on_hyphens=False)
                for entry in entries]
            lines.append("\n".join(block) + "\n")
    return "\n".join(lines)


def write_changes(releases):
    content = {"releases": {v: releases[v] for v in order(releases)}}
    CHANGES.write_text(
        yaml.safe_dump(content, sort_keys=False, default_flow_style=False,
                       allow_unicode=True, width=10 ** 6),
        encoding="utf-8")


def command_lint(args):
    status = 0

    def report(message):
        nonlocal status
        print(f"error: {message}")
        status = 1

    for path in fragment_paths():
        lint_fragment(path, report)

    releases = load_releases()
    for version in releases:
        if not VERSION.match(version):
            report(f"changelog.yaml: '{version}' is not a version")

    if DOCUMENT.is_file():
        if DOCUMENT.read_text(encoding="utf-8") != render(releases):
            report("CHANGELOG.md does not match changelogs/changelog.yaml. "
                   "The file is generated, so do not edit it by hand.")
    else:
        report("CHANGELOG.md is missing")

    if status == 0:
        print("changelog: ok")
    return status


def command_release(args):
    version = args.version
    if not VERSION.match(version):
        print(f"error: '{version}' is not a version of the form X.Y.Z")
        return 1

    status = 0

    def report(message):
        nonlocal status
        print(f"error: {message}")
        status = 1

    paths = fragment_paths()
    if not paths:
        print("error: changelogs/fragments/ holds no fragment")
        return 1

    changes = {}
    for path in paths:
        clean = lint_fragment(path, report)
        if clean is None:
            continue
        for section, entries in clean.items():
            if section == TRIVIAL:
                continue
            changes.setdefault(section, []).extend(entries)
    if status:
        return 1

    summaries = changes.get("release_summary", [])
    if len(summaries) > 1:
        print(f"error: {len(summaries)} fragments hold a release_summary, and "
              "a release takes 1")
        return 1
    if not changes:
        print("error: every fragment is trivial, so the release has no notes")
        return 1

    for section in changes:
        if section != "release_summary":
            changes[section].sort()

    releases = load_releases()
    if version in releases:
        print(f"error: {version} is already released")
        return 1

    releases[version] = {
        "release_date": args.date or datetime.date.today().isoformat(),
        "changes": {s: changes[s] for s, _ in SECTIONS if s in changes},
    }

    write_changes(releases)
    DOCUMENT.write_text(render(releases), encoding="utf-8")
    for path in paths:
        path.unlink()

    print(f"released {version}")
    print(f"  wrote {rel(CHANGES)} and {rel(DOCUMENT)}")
    print(f"  removed {len(paths)} fragments")
    return 0


def command_notes(args):
    releases = load_releases()
    release = releases.get(args.version)
    if release is None:
        print(f"error: {args.version} is not in changelogs/changelog.yaml",
              file=sys.stderr)
        return 1

    changes = release.get("changes") or {}
    out = []
    for section, title in SECTIONS:
        entries = changes.get(section)
        if not entries:
            continue
        if section == "release_summary":
            out.append(entries[0] + "\n")
            continue
        out.append(f"### {title}\n")
        out.append("\n".join(f"- {entry}" for entry in entries) + "\n")
    print("\n".join(out))
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)

    commands.add_parser("lint", help="check the fragments and CHANGELOG.md")

    release = commands.add_parser("release", help="compile the fragments")
    release.add_argument("--version", required=True, help="X.Y.Z")
    release.add_argument("--date", help="the release date, default today")

    notes = commands.add_parser("notes", help="print the notes of a release")
    notes.add_argument("--version", required=True, help="X.Y.Z")

    args = parser.parse_args()
    return {
        "lint": command_lint,
        "release": command_release,
        "notes": command_notes,
    }[args.command](args)


if __name__ == "__main__":
    sys.exit(main())
