#!/bin/sh
# Checks that the gate job of each workflow depends on every other job.
set -eu

cd "$(dirname "$0")/.."

python3 - <<'PY'
import pathlib
import sys

import yaml

status = 0
paths = sorted(pathlib.Path(".github/workflows").glob("*.y*ml"))
if not paths:
    print("error: the repository has no workflow")
    sys.exit(1)

for path in paths:
    jobs = yaml.safe_load(path.read_text())["jobs"]
    if "gate" not in jobs:
        print(f"{path}: the workflow has no gate job")
        status = 1
        continue
    missing = sorted(set(jobs) - {"gate"} - set(jobs["gate"].get("needs", [])))
    if missing:
        names = ", ".join(missing)
        print(f"{path}: the gate job does not depend on {names}")
        status = 1

if status == 0:
    print("workflows: ok")
sys.exit(status)
PY
