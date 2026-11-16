<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Restore Pod contract

A Pod requests restore by naming a ready `PodSnapshot` in its namespace. The
Snapshot node agent restores only Pods that implement this contract.

Go integrations should use:

```go
restored, err := podcontract.Build(
    pod,
    podcontract.Request{
        SnapshotName:    snapshotName,
        SourceContainer: checkpointedContainer,
        Mappings:        mappings,
    },
    podcontract.Options{
        SeccompProfile:   podcontract.DefaultSeccompLocalhostProfile,
        EnableStartupGate: true,
    },
)
```

This release intentionally moves the Go Pod-contract API from
`api/restorepod` and `api/v1alpha1` to `api/podcontract` without compatibility
aliases. Consumers must update restore annotations, mappings, status reasons,
`RestoreOutcome`, and `ClassifyRestoreOutcome` references to the new package.
Custom-resource types remain in `api/v1alpha1`.

`podcontract.Build` is a pure, atomic transformation: it returns a deep copy or
an error and never mutates its input. Reapplying it with the same arguments is
idempotent. It emits one canonical representation and validates that exact
output. `podcontract.Validate` checks only Snapshot's minimum restore protocol,
without mutation or Kubernetes API reads. Optional workload policy such as
startup gating and seccomp selection remains producer-owned. Conflicting
annotations, volumes, mounts, environment, and requested security settings are
rejected instead of overwritten.

Consumers can interpret the agent-owned `nvidia.com/Restored` Pod condition
without importing Snapshot's custom-resource API:

```go
outcome := podcontract.ClassifyRestoreOutcome(pod.Status.Conditions)
```

The package also owns the shared control-volume, environment, checkpoint-sentinel,
and CUDA job-file names used by Snapshot-managed source and restore Pods.

The producer supplies `SourceContainer` from the referenced `PodSnapshot`.
Empty mappings restore that source into the same-named destination only. The
builder validates one-source-to-many-destination consistency but deliberately
performs no API read to discover the checkpointed source.

Non-Go producers can implement the declarative form directly. This fan-out
example restores the checkpointed `main` process into two destination containers:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: restored-worker
  annotations:
    nvidia.com/restore-from: worker-snapshot
    nvidia.com/restore-container-map: main=engine-0,main=engine-1
spec:
  volumes:
    - name: snapshot-control
      emptyDir: {}
  containers:
    - name: engine-0
      image: worker:latest
      securityContext:
        seccompProfile:
          type: Localhost
          localhostProfile: profiles/block-iouring.json
      # The source's command. Snapshot replaces it with its restore proxy.
      command: ["/app/worker"]
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: engine-0
      startupProbe:
        exec:
          command: ["cat", "/snapshot-control/restore-complete"]
        timeoutSeconds: 1
        periodSeconds: 1
        failureThreshold: 1800
        successThreshold: 1
    - name: engine-1
      image: worker:latest
      securityContext:
        seccompProfile:
          type: Localhost
          localhostProfile: profiles/block-iouring.json
      command: ["/app/worker"]
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: engine-1
      startupProbe:
        exec:
          command: ["cat", "/snapshot-control/restore-complete"]
        timeoutSeconds: 1
        periodSeconds: 1
        failureThreshold: 1800
        successThreshold: 1
```

The container mapping is optional for a single same-name restore. Every
destination mounts the one shared `snapshot-control` `emptyDir` at
`/snapshot-control` with `subPath` equal to its container name. The canonical
`SNAPSHOT_CONTROL_DIR` environment variable points to that mount. It is a
required part of the minimum contract so every Snapshot-managed workload and
tool can discover the control directory without hard-coding its location. The
builder also injects the deprecated `DYN_SNAPSHOT_CONTROL_DIR` alias during
the migration window; hand-authored Pods may omit the alias.

The `restore-complete` startup probe shown above is optional workload lifecycle
policy. Set `Options.EnableStartupGate` when Kubernetes must withhold readiness
and liveness until restore completes. Because Kubernetes supports only one
startup probe, the builder then replaces any existing startup probe with the
canonical restore-completion gate while preserving workload liveness and
readiness probes. Its failure threshold allows 1,800 consecutive one-second
failures; if restoration exceeds that budget, kubelet kills the container, and
the restore is lost (see restart policy below). The node agent always writes
the sentinel but does not require or inspect the optional probe.

`Options.SeccompProfile` controls the localhost profile on restore destination
containers only. The standard Snapshot Helm installation deploys
`DefaultSeccompLocalhostProfile` to block io_uring for CRIU. Applying it at
container scope preserves the Pod-level policy inherited by unrelated
sidecars. An empty value leaves seccomp unmanaged for environments that provide
the restriction elsewhere. A destination container must not override a
requested profile with a conflicting profile.

Snapshot replaces the command of every destination container with its restore
proxy, `/snapshot-control/snapshot-restore-proxy`. The Pod object is not
changed: the node agent's NRI plugin sets the process arguments when the
container is created. That needs NRI v0.10 or newer, enabled in the container
runtime: containerd 2.2 or newer. The proxy is the container's PID 1. It waits
for the restore, then follows the restored process: it forwards signals to it,
and exits with its exit code, so the container ends when the restored workload
does. If the proxy is not PID 1, the agent fails the restore. Keep the source's
command in restore Pods; it never runs. Other containers in the Pod, such as
sidecars, are left alone. Nodes whose runtime cannot run the proxy restore
without it; see [Nodes without the restore proxy](#nodes-without-the-restore-proxy).

The plugin decides which containers get the proxy when each container is
created. Without `restore-container-map`, it reads the PodSnapshot that
`restore-from` names, for the checkpointed container's name. Create the restore Pod
after that PodSnapshot exists; it does not have to be Ready yet. Until it
exists, container creation fails, and kubelet retries with a backoff of up to 5
minutes. A Pod with `restore-from-map` names its destinations itself, so it can
be created before its PodSnapshots exist.

Probes belong to the workload. Kubernetes runs liveness and readiness probes
from container start, while the restore is still running and nothing in the
container answers them yet. A liveness probe that fails during that time kills
the container and loses the restore. Either set `Options.EnableStartupGate`,
which holds liveness and readiness back until `restore-complete` exists, or
give liveness probes an `initialDelaySeconds` or `failureThreshold` that covers
the longest restore you expect.

Restore Pods are expected to use `restartPolicy: Never`. A Pod gets one restore:
a container that starts again in the same Pod finds that restore already used,
and its proxy exits with code 112. With `restartPolicy: Always` or `OnFailure`,
as in a Deployment, the container then ends in `CrashLoopBackOff` once the
restored workload exits. Delete the Pod to restore again.

A restore that does not run ends like one that failed: the agent writes
`restore-failed` into each destination's control directory and kills the
container. That happens when the compatibility checks refuse the restore, when
its preflight fails, and when its PodSnapshots are not ready to restore
`config.restore.snapshotWaitSeconds` (default 60) after the first destination
container started. A destination container that starts later finds
`restore-failed` when it starts. The agent never restores into a container
whose control directory holds `restore-failed`, whoever wrote it.

The `nvidia.com/Restored` condition tells these apart. A refusal is
`RestoreIncompatible`, and a restore whose PodSnapshots were not ready in time
is `RestoreSnapshotNotReady`. When every destination's own workload had
written `restore-failed` before the agent began, the restore is
`RestoreEndedByWorkload`, and when every destination's workload had claimed
its container first ([Claiming the container](#claiming-the-container)), it is
`RestoreClaimedByWorkload`. A restore refused because a destination container
was not started with the snapshot's inputs is `RestoreInputsChanged`
([Restore only with the snapshot's inputs](#restore-only-with-the-snapshots-inputs)).
The agent writes `owner=agent` into the
`restore-failed` it writes, which is how it tells its own marker from the
workload's; a workload that writes the file should write anything else. All
five classify as `Failed` in `podcontract.ClassifyRestoreOutcome`: nothing was
restored.

The proxy reserves two exit codes:

- 112 (`RestoreNotRepeatableExitCode`): the Pod's one restore is used up. The
  restore failed, or an earlier container in the Pod already used it.
- 113 (`RestoreProxyErrorExitCode`): the proxy could not follow the restored
  process.

Every other exit code is the restored workload's own, or 128 plus the number of
the signal that ended it.

## Restore from several PodSnapshots

A PodSnapshot checkpoints one container. To restore several containers of one
Pod from different PodSnapshots, such as the prefill and decode containers of a
disaggregated serving unit, set `nvidia.com/restore-from-map` instead of
`restore-from`. Its value is a comma-separated list of
`destination=snapshot` pairs: the checkpointed container of each named
PodSnapshot, in the Pod's namespace, restores into that destination container.

- A Pod that sets `restore-from-map` together with `restore-from` or
  `restore-container-map` fails its restore.
- Each destination appears once. Two destinations may name the same
  PodSnapshot, which restores it into both.
- The map names at most 8 destinations (`podcontract.MaxSnapshotMappings`).
- Each destination meets the same requirements as above: the
  `snapshot-control` mount with `subPath` equal to its name, and
  `SNAPSHOT_CONTROL_DIR`. Other containers start normally.

The restore waits until every PodSnapshot is Ready and its snapshot is
available, then restores all destinations in parallel. A PodSnapshot that
failed, that this node cannot run, or that is not ready within
`config.restore.snapshotWaitSeconds`, ends the restore before any destination
is restored. The `nvidia.com/Restored` condition reports the destinations
together, with the same reasons as fan-out, and its messages name each
destination's PodSnapshot. A Pod whose restore partially succeeded keeps the
destinations that restored running; delete it to restore again.

Go integrations set `Request.SnapshotMappings` instead of `SnapshotName`,
`SourceContainer`, and `Mappings`:

```go
restored, err := podcontract.Build(pod, podcontract.Request{
    SnapshotMappings: []podcontract.SnapshotMapping{
        {Destination: "prefill", SnapshotName: "prefill-snapshot"},
        {Destination: "decode", SnapshotName: "decode-snapshot"},
    },
}, options)
```

This example restores two PodSnapshots into one Pod, beside a sidecar that
starts normally:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: restored-prefill-decode
  annotations:
    nvidia.com/restore-from-map: decode=decode-snapshot,prefill=prefill-snapshot
spec:
  volumes:
    - name: snapshot-control
      emptyDir: {}
  containers:
    - name: prefill
      image: worker:latest
      securityContext:
        seccompProfile:
          type: Localhost
          localhostProfile: profiles/block-iouring.json
      # The source's command. Snapshot replaces it with its restore proxy.
      command: ["/app/worker", "--role=prefill"]
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: prefill
      startupProbe:
        exec:
          command: ["cat", "/snapshot-control/restore-complete"]
        timeoutSeconds: 1
        periodSeconds: 1
        failureThreshold: 1800
        successThreshold: 1
    - name: decode
      image: worker:latest
      securityContext:
        seccompProfile:
          type: Localhost
          localhostProfile: profiles/block-iouring.json
      command: ["/app/worker", "--role=decode"]
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: decode
      startupProbe:
        exec:
          command: ["cat", "/snapshot-control/restore-complete"]
        timeoutSeconds: 1
        periodSeconds: 1
        failureThreshold: 1800
        successThreshold: 1
    - name: router
      image: router:latest
```

## Restore only with the snapshot's inputs

Kubernetes resolves a container's environment variables from Secrets and
ConfigMaps, and expands `$(VAR)` references in its arguments, when the
container starts. A restored process keeps the values its container was
started with when the snapshot was taken. If a Secret changes between the
checkpoint and the start of a restore Pod's container, the container is given
the new value while the restored process holds the old one.

A restore Pod can ask the agent to restore a destination container only if it
was started with the same inputs, with the `nvidia.com/restore-match-inputs`
annotation. Its value is a JSON object keyed by destination container:

```yaml
metadata:
  annotations:
    nvidia.com/restore-from: worker-snapshot
    nvidia.com/restore-match-inputs: '{"main": {"env": ["DB_PASSWORD", "API_URL"], "args": true}}'
```

- `env` names environment variables whose values must equal the snapshot's. A
  variable the snapshot's container did not have must be absent too. List only
  variables that are the same in every Pod that restores this snapshot: not the
  Pod's name or IP, `HOSTNAME`, or anything read with the downward API.
- `args: true` also compares the container's arguments, after Kubernetes
  expanded them. The restore proxy replaces a container's arguments, so it can
  be compared only on [nodes without the restore proxy](#nodes-without-the-restore-proxy).
- An entry names at least one input. The annotation holds names only; never
  put a value in it.

A checkpoint records, for every snapshot, a salted SHA-256 digest of each of
the target container's environment variables and of its arguments, in the
snapshot's own files. The values themselves are already in the snapshot, in the
memory of the process it saved; the digests are not published anywhere else.
`status.source.pod.recordedInputs` on the `PodSnapshotContent` lists `Env` and
`Args` when they were recorded.

After the agent claims a destination container and before it touches it, its
GPUs, or the snapshot, it compares the digests of the named inputs in the
container's runtime spec with the recorded ones. It refuses the restore when
they differ, when the snapshot recorded none (it was taken by an older agent),
when the record or the annotation cannot be read in full, or when the
annotation names a container that is not a destination. The refusal ends the
destination like an incompatible restore: `restore-failed` and a kill, so the
container starts again at once, without a restore, with the values it is given
now. The `nvidia.com/Restored` condition is `RestoreInputsChanged`, and its
message names the inputs, never their values.
`nvidia.com/snapshot-skip-compat-check` does not skip this check. Destinations
the annotation does not name are not checked, and a Pod without it restores as
before. A node agent older than this check ignores the annotation, so upgrade
the agents before you rely on it.

Files from Secret and ConfigMap volumes are not compared yet: Kubernetes
mounts their current content, while the restored process may hold what it
read before the checkpoint.

## Nodes without the restore proxy

On a node whose container runtime has no NRI, or NRI older than v0.10
(containerd older than 2.2, or NRI disabled), the agent restores without the
proxy. So does every agent of an install with `daemonset.restoreProxy.enabled:
false`, which keeps the agent out of container creation on its nodes. Its log says so when it starts. The container's own command stays PID 1,
and the agent restores the workload beside it. On such nodes:

- Give every destination container an inert, long-running command, such as
  `["sleep", "infinity"]`. The source's command would start the workload from
  scratch next to the restored one. An inert command also works on nodes with
  the proxy, which replaces it.
- Add a liveness probe that checks the workload itself, such as its API.

Without the proxy:

- The container does not end when the restored workload exits, and the
  workload's exit code is lost. The Pod can stay Running and Ready with no
  workload; the liveness probe is what notices.
- Signals reach the inert command, not the workload. When the Pod is deleted,
  the workload gets no `SIGTERM`; it is killed at the end of the grace period.
- After a failed or refused restore, the agent kills the container. If it
  starts again, it runs the inert command and is not restored. Delete the Pod
  to restore again. A command that starts the workload itself when no restore
  comes can watch for `restore-failed`. When it gives up waiting on its own, it
  must first claim the container, as below, and then write `restore-failed`
  before it starts the workload, so that a restore that becomes possible later
  stays out of the container.
- The proxy's exit codes, 112 and 113, do not apply.

### Claiming the container

A command that starts the workload itself when no restore comes races the
agent: if it gives up just as the agent begins the restore, the restore would
run into a container that already runs the workload, and two copies of it would
share the container's GPUs. `restore-claim` in the destination's control
directory settles the race. Whoever creates it first owns the container until
the Pod is deleted:

- Create it whole: write `owner=workload` into a temporary file in the same
  directory, then hard-link that file to `restore-claim` (`link(2)` or
  `linkat(2)`) and remove the temporary file. The link fails with `EEXIST` if
  anything exists under the name, so it decides the claim as an exclusive
  create would, and the claim is never seen empty. Nothing else decides the
  claim: only whether your link succeeded.
- Nobody removes, renames or rewrites it. It outlives the container's
  restarts, since the control directory is the Pod's `emptyDir`. The agent
  clears `restore-complete` and `restore-failed` before each restore, never
  `restore-claim`.
- The agent claims a destination after it has checked for `restore-failed`
  and before it touches the container or its GPUs for the restore, and before
  it ends a container with `restore-failed` and a kill. Its claim holds
  `owner=agent` and the container's runtime ID (`container=<id>`). It restores
  only into a container whose claim it created, or created before for the
  same container, after an agent restart.
- The content is `key=value` lines; spaces around keys and values are
  ignored, and so are unknown keys. `owner` is exactly `agent` or `workload`.
- If your link fails because the file exists, and it does not say
  `owner=workload`, the agent owns the container: do not start the workload.
  Keep waiting for `restore-complete` or `restore-failed`, with a limit of your
  own; past it, write `restore-failed` and exit, so the container's restart
  starts the workload. Treat an empty or unreadable claim the same way. A
  claim that says `owner=workload` is yours, from an earlier container of the
  Pod.
- If the workload claimed the container first, the agent neither restores
  into it nor kills it, and writes nothing into it. The restore ends with reason
  `RestoreClaimedByWorkload` when every destination was claimed that way.
- A claim the agent made for an earlier container of the Pod, or one it cannot
  read (empty, or an `owner` other than `agent` or `workload`, after reading
  it again for up to 100 ms), keeps the restore out of the container too. The
  agent then writes `restore-failed`, unless the container holds one, so that
  what waits there stops waiting, but does not kill the container; the restore
  is `RestoreFailed`.
- A container that holds `restore-failed` is not restored. It counts as
  `RestoreEndedByWorkload` only when the claim is the workload's or there is
  none; when the agent had claimed the container, a workload that stopped
  waiting for that restore ended a restore that did not finish, and the
  restore is `RestoreFailed`.
- After an agent restart, `restore-complete` says the restore finished only
  when the agent's claim names the same container (or there is no claim, from
  an agent before claims); one left by a restore into an earlier container of
  the Pod is not taken for this one.
- The restore proxy never claims a container, so with the proxy the agent's
  claim always succeeds and nothing changes.

The limit on your wait after the agent claimed bounds the restore: once it
passes, your container exits and takes any restored process with it, and the
restore fails. Make it longer than a restore takes;
`config.restore.timeoutSeconds` is the agent's own bound.
