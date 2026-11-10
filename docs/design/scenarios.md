# Scenarios — design

A scenario is a versioned document and a run of that document. The
document declares machines, ordered steps, asserts, cleanup and
evidence. The run drives the lab under harness leases.

[`cluster.md`](cluster.md) is the product shape. This file is the
document of the scenario interface. Machine create and desired
hardware stay in [`cluster.md`](cluster.md) section 4. The console
wall, the lease and interactive serial stay in [`labview.md`](labview.md).

Status: design with a v1 runner under
https://github.com/maglo/qemu-lab-manager/issues/88.
The document format was locked under
https://github.com/maglo/qemu-lab-manager/issues/82.

---

## 1. Scope

In:

- Versioned documents with `apiVersion` and `kind`.
- A `Scenario` document the API accepts as a run.
- A runner that takes harness leases, runs steps in order, asserts,
  captures evidence and cleans up.
- Scenario events on the telemetry stream.
- A CLI that posts the same documents the API accepts.

Out:

- Desired hardware fields (firmware, TPM, readers, disk layout). Those
  belong on the machine create and PATCH documents.
- Server-side expect on interactive serial attach. Matching lives only
  in the scenario runner.
- OTLP.
- Creation of a host.

---

## 2. Why three objects

| Object | Owns | Document `kind` |
|---|---|---|
| Machine desired | Durable identity and hardware the LCM reconciles | `Machine` (and a `Lab` list of machines) |
| Transition | One action on a live machine | A step inside a `Scenario` |
| Scenario | One run: machines, steps, asserts, cleanup, evidence | `Scenario` |

A "state" that mixes hardware and timed actions is two things. Keep
them apart. The runner never invents QEMU flags. It creates or reuses
machines through the LCM API, then calls the same power, keys,
screenshot and hotplug endpoints a human uses.

---

## 3. Versioning

Every document carries:

```yaml
apiVersion: labview.dev/v1
kind: Scenario
```

| Rule | Meaning |
|---|---|
| Additive in a major | New optional fields only. |
| Breaking change | New major (`v2`). Rename, remove, or change a default that changes QEMU or step meaning. |
| Unknown `apiVersion` | The server refuses the document. |
| Dual-read | The server accepts the current major and the previous major while a migration runs. |
| No silent rewrite | Applying a `v1` file does not rewrite it as `v2` on disk. |
| Run records version | Evidence and telemetry store the `apiVersion` of the document that ran. |

`Lab`, `Machine` and `Scenario` share the `labview.dev/vN` line. A
major bump can touch one kind and leave the others unchanged in shape,
but the version string still moves together so a client pins one line.

Unknown fields on a document are an error. Extensions use an
`x-` prefix on field names if a lab must carry private data; the
runner ignores `x-` fields.

YAML is the in-repo form for humans. JSON is the wire form. The two
encode the same schema.

---

## 4. Scenario document

```yaml
apiVersion: labview.dev/v1
kind: Scenario
metadata:
  name: boot-order-smoke
spec:
  leaseAs: harness/boot-order/${RUN_ID}
  machines:
    - use: target-a
    # or: create: { id: target-a, memoryMb: 2048, vcpus: 2, diskGb: 20 }
  steps:
    - power: { machine: target-a, op: start }
    - waitSerial: { machine: target-a, match: "login:", timeout: 60s }
    - keys: { machine: target-a, sequence: ["ret"] }
    - assertSerial: { machine: target-a, match: "login:", timeout: 10s }
    - evidence:
        screenshot: [target-a]
        serial: [target-a]
  cleanup: destroy
```

### Fields

| Field | Meaning |
|---|---|
| `metadata.name` | Short name for the run list and telemetry. |
| `spec.leaseAs` | Lease holder identity. Must match `harness/<name>/<run-id>`. |
| `spec.machines` | Create through LCM, or reuse an id that already exists. |
| `spec.steps` | Ordered list. One action or assert per step. |
| `spec.cleanup` | `destroy` or `keep` after the last step (also after failure). |

`${RUN_ID}` expands when the run starts. The server assigns a run id
if the client does not send one.

### Machines in a scenario

- `use: <id>` — the machine must already exist.
- `create: <object>` — the object is the LCM create document. The
  runner posts it to `POST /api/machines` before steps run.

A scenario does not grow its own hardware schema. When LCM adds
firmware or TPM fields, `create` gains them through that document.

---

## 5. Steps (v1)

v1 steps call endpoints that already ship, plus matching the runner
owns.

| Step | Meaning | Needs |
|---|---|---|
| `power` | `start`, `stop` or `restart` | Write lease |
| `waitSerial` | Read serial until `match` or timeout | Serial broker |
| `keys` | Send one chord or a sequence of chords | Write lease, QMP |
| `assertSerial` | Like `waitSerial`; failure fails the run | Serial broker |
| `assertScreen` | Capture a screenshot; optional future image assert | Screenshot API |
| `evidence` | Copy screenshot and serial capture into the run bundle | — |
| `httpGet` | GET a guest URL the host can reach; assert status | Guest network |

Later steps (not v1): `hotplug` (USB, smart card), guest filesystem
copy, firmware UI helpers. Those land when LCM exposes the verbs.

### Matching

Only the scenario runner matches serial bytes. Interactive
`/ws/serial/{id}` stays dumb, as [`labview.md`](labview.md) section 5
requires. Timeouts live on the step. A timed-out wait fails the step.

---

## 6. Evidence

A run writes a bundle under the node's data directory, one directory
per run id:

- `scenario.json` — the document that ran, with `apiVersion`.
- `result.json` — pass or fail, per-step outcomes, timestamps.
- Screenshots as PNG, named by step and machine.
- Serial captures (asciicast v2), named by machine.
- Pointers into the activity log for the run window.

The bundle reuses the screenshot and serial transcript paths that
already ship. It does not invent a second capture pipeline.

---

## 7. HTTP surface

| Method | Path | Meaning |
|---|---|---|
| `POST` | `/api/scenarios/runs` | Start a run from a Scenario document. |
| `GET` | `/api/scenarios/runs` | List recent runs. |
| `GET` | `/api/scenarios/runs/{id}` | One run: status, step, result. |
| `POST` | `/api/scenarios/runs/{id}/cancel` | Stop a run; then run cleanup. |
| `GET` | `/api/scenarios/runs/{id}/evidence/{name}` | One evidence file. |

`POST /api/scenarios/runs` takes the write leases named in the
document (or fails if another holder has one). The runner renews
leases for the life of the run.

`-scenarios-dir` holds evidence bundles on disk. Empty keeps evidence
names on the run record only. The runner stays on either way.

These paths are also in [`labview.md`](labview.md) section 5.

---

## 8. Telemetry

Slice 5 streams node and machine events. Scenarios add:

| Kind | When |
|---|---|
| `scenario.run.started` | Run accepted |
| `scenario.step.started` | Step begins |
| `scenario.step.finished` | Step ends (pass or fail) |
| `scenario.run.finished` | Run ends (pass, fail or cancel) |

Each event carries `runId`, `apiVersion`, `name` and `step` when
relevant. The payload shape follows the telemetry slice.

---

## 9. CLI

A thin client over the same documents. It does not embed QEMU logic.

```text
labviewctl apply -f lab.yaml
labviewctl run -f scenario.yaml
labviewctl get run <id>
labviewctl evidence <id> -o ./out
```

`labviewctl run` posts JSON to `POST /api/scenarios/runs` and follows
status. Manual operators and CI use one schema. CI sets
`leaseAs: harness/<suite>/<run-id>`.

The binary name and whether it is a subcommand of `labview` or a
second binary land with the implementation. The document interface
does not depend on that choice.

---

## 10. Functional, non-functional, adversarial

One runner. Suites differ by steps and asserts:

- Functional — boot, enroll, reboot, assert serial or HTTP.
- Non-functional — budgets on step duration; caps from telemetry.
- Adversarial — unexpected media, power loss, negative asserts.

Adversarial cases need hotplug and richer desired hardware from LCM.
They do not need a second engine.

---

## 11. Runner placement

The first runner lives in-process in labview on the API node. It calls
local machine endpoints and, after cluster join, peer-forwarded ones.

A helper binary is allowed later if the in-process runner becomes a
poor fit. The document and HTTP surface stay the same.

---

## 12. Open follow-ons

Locked: `apiVersion` / `kind`, YAML in-repo and JSON on the wire,
uploaded runs, matching only in the runner, shared version line for
`Lab` / `Machine` / `Scenario`, evidence bundle shape, HTTP paths,
telemetry kind/type names (`scenario` + `run.started` and kin),
`-scenarios-dir` for bundles.

Still open:

1. Published JSON Schema file beside the docs.
2. Whether `labviewctl` is a subcommand or a second binary.
3. Default `-scenarios-dir` on the node image.
