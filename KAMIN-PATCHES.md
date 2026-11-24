# Patches carried by this fork

`kamin/main` is upstream [ai-dynamo/snapshot](https://github.com/ai-dynamo/snapshot)
`v0.3.0` plus patches 1–6 below, one commit each, in this order, then one
`ci(kamin)` commit (`.github/workflows/kamin-release.yaml`,
`.github/kamin/package-chart.sh`) and this file, then patches 7 and 8 (with
patch 8's joint race test), patch 9 and patch 10. Patch 10 adds the changes
to CRIU that the agent image carries (`agent/criu-patches/`) and the fork's
own pull request checks (`.github/workflows/kamin-checks.yaml`). Nothing else
differs from upstream. Every patch is meant to go upstream, the CRIU patches
to CRIU; the fork exists until they are merged there.

Status as of 2026-11-24.

| # | Commit | Change | Upstream |
|---|--------|--------|----------|
| 1 | `c297601a` | restore one Pod's containers from several PodSnapshots (`nvidia.com/restore-from-map`) | not covered; overlaps the open pod-group API design (#612, #811) |
| 2 | `81d92cfb` | take the restore Pod's IP from its CRI sandbox before kubelet publishes it | not covered |
| 3 | `c832346a` | CRIU LZ4 memory page compression and parallel decoding | not covered; complements #573 |
| 4 | `cd86f4d3` | chart value `daemonset.restoreProxy.enabled` | not covered; naming at risk from #893 |
| 5 | `efa13477` | capture a Pod's containers one after the other | not covered |
| 6 | `dfcf8c2e` | end a restore that will not run (refusal, `config.restore.snapshotWaitSeconds`) | not covered; overlaps open designs #800 (SNEP-322), #836 (SNEP-247) |
| 7 | `3038962f` | own condition reasons for restores that did not run (`RestoreSnapshotNotReady`, `RestoreEndedByWorkload`) | not covered; reasons may collide with the restore-retry stack (#899, #902) |
| 8 | `41161579`, `df4f9e29`, `7b37a543` | claim a destination container (`restore-claim`) before restoring into it or ending it | not covered; conflicts with SNEP-883 (#884), touches SNEP-247 (#836) and SNEP-322 (#800) |
| 9 | `0ff9210b` | restore only into a container started with the snapshot's inputs (`nvidia.com/restore-match-inputs`, `RestoreInputsChanged`) | not covered; reason may collide with the restore-retry stack (#899, #902) |
| 10 | `0fc5cb7c` | build CRIU with a patch that picks another link remap name when the name is taken, so containers of one Pod that share `/dev/shm` can be captured | CRIU: not covered; Snapshot: not covered |

None of the patches has an upstream issue or pull request yet.

## Releases

A release is a tag `v<upstream>-kamin.<n>` on a `kamin/main` commit: `<upstream>`
is the upstream release the branch is built on, `<n>` counts the fork's
releases on it from 1. The tag publishes, to this repository's GHCR namespace:

- the images `ghcr.io/fishaudio/snapshot/{agent,operator,pagebroker}:v<upstream>-kamin.<n>`,
  linux/amd64. They are the images the branch push built for that commit,
  tagged again, so the digest of the version tag equals that of the commit tag;
- the chart `oci://ghcr.io/fishaudio/snapshot/charts/snapshot`, version
  `<upstream>-kamin.<n>`, appVersion `v<upstream>-kamin.<n>`. Its image
  repositories default to `ghcr.io/fishaudio/snapshot/*` and its image tags to
  appVersion, so the chart installs the images of its own version. The chart
  in git stays as upstream ships it; `.github/kamin/package-chart.sh` makes
  these changes in a staged copy.

Every push to `kamin/main` also publishes the images under the 12-character
commit, for trying a commit before it is released.

The version is a SemVer prerelease, so Helm needs it spelled out:

```sh
helm upgrade --install snapshot oci://ghcr.io/fishaudio/snapshot/charts/snapshot \
  --version 0.3.0-kamin.1 --namespace <namespace>
```

### Cutting a release

1. Push the commit to `kamin/main` and wait for its `kamin release` run to
   succeed: the release reuses that run's images.
2. Tag the same commit and push the tag:

   ```sh
   git tag -a v0.3.0-kamin.1 -m "v0.3.0-kamin.1" <commit>
   git push fishaudio v0.3.0-kamin.1
   ```

3. The tag's `kamin release` run checks that the commit is on `kamin/main`,
   tags the commit's images with the version (it builds them only if they are
   missing) and pushes the chart, then reads the chart back and checks the
   images it names.
4. From outside, without credentials, check that the chart and the images can
   be pulled and that each version tag has the digest of its commit tag:

   ```sh
   helm show chart oci://ghcr.io/fishaudio/snapshot/charts/snapshot --version 0.3.0-kamin.1
   helm template snapshot oci://ghcr.io/fishaudio/snapshot/charts/snapshot \
     --version 0.3.0-kamin.1 | grep -o 'ghcr.io/fishaudio/snapshot/[a-z]*:[^" ]*' | sort -u
   docker buildx imagetools inspect ghcr.io/fishaudio/snapshot/agent:v0.3.0-kamin.1
   ```

   The first release creates the GHCR package `snapshot/charts/snapshot`; if
   it is private, make it public in the package settings.

Do not publish a GitHub Release for the tag: upstream's `release.yaml` runs on
`release: published` and would try to publish to upstream's registry.

### Moving to a newer upstream

Rebuild `kamin/main` on the new upstream tag: apply the patches still needed,
drop those upstream has merged, keep the `ci(kamin)` commit and this file, and
start the count again (`v0.4.0-kamin.1`). Update this file in the same change.

## 1. Restore one Pod's containers from several PodSnapshots

Commit `c297601a` `feat(restore): restore one Pod's containers from several PodSnapshots`.

- **What**: a restore Pod annotation `nvidia.com/restore-from-map`, a
  comma-separated list of `destination=snapshot` pairs (at most 8). The
  checkpointed container of each named PodSnapshot is restored into its
  destination container; two destinations may name the same PodSnapshot. A
  Pod that also sets `nvidia.com/restore-from` or
  `nvidia.com/restore-container-map` is rejected. The agent runs the existing
  preflight once per PodSnapshot (any terminal error fails the Pod, any
  pending one keeps it pending), then restores every destination in parallel
  from its own artifact. Recovery after an agent restart is per destination.
  `podcontract.Request.SnapshotMappings`, the NRI plugin, the operator's source
  Job check, `snapshotctl restore` and the e2e helpers understand it. Pods
  without the annotation behave as before.
- **Why**: a PodSnapshot checkpoints one container, and a restore Pod could
  name one PodSnapshot. A Pod whose GPU containers are checkpointed separately,
  such as the prefill and decode containers of a disaggregated serving unit,
  could not be restored as one Pod.
- **Files**: `api/podcontract/{protocol,restore_pod}.go`,
  `agent/internal/controller/{restore_from_map,controller,compat}.go`,
  `agent/internal/nri/plugin.go`, `operator/cmd/snapshotctl/{main,restore}.go`,
  `operator/internal/protocol/source_job.go`,
  `e2e/snapshot_e2e/framework_workloads.py`, docs (`restore-pod-contract.md`,
  `api.md`, `cli.md`, `architecture/README.md`, snapshotctl README),
  `CHANGELOG.md`.
- **Tests**: `restore_from_map_test.go` (preflight per PodSnapshot, shared
  PodSnapshot read once, pending/failed/missing artifact/incompatible, parallel
  restore, partial and total failure, per-destination recovery, tally
  messages), `nri/plugin_test.go`, `podcontract/{protocol,restore_pod}_test.go`
  (parsing, limit, idempotent `Build`, conflicts, drift),
  `snapshotctl/restore_test.go`, `protocol/source_job_test.go`. No existing
  test changed.
- **Upstream**: not covered. Upstream restores one PodSnapshot into one or
  more containers (`restore-from`, `restore-container-map`, #113), never
  several PodSnapshots into one Pod. The pod-group API
  ([#612](https://github.com/ai-dynamo/snapshot/issues/612), approved;
  [#811](https://github.com/ai-dynamo/snapshot/pull/811), open) restores one
  member snapshot per target Pod and caps container lists at one entry, so it
  does not cover this, but it reshapes the same code (`podcontract` `Build`,
  `restore_pod.go`) and a maintainer will ask how the two fit.
  [#847](https://github.com/ai-dynamo/snapshot/issues/847) (prefill and decode
  restored independently) is the closest use case.
  [#875](https://github.com/ai-dynamo/snapshot/issues/875) (reject restore
  annotations on a SnapshotJob) lists only the two existing annotations.
- **Against upstream main**: the code applies cleanly; the docs conflict with
  the docs restructure (#786: architecture overview moved, `api.md`, `cli.md`,
  `restore-pod-contract.md`, the `snapshotctl` README cut to a link). Open PRs
  #811, #913, #934 (`podcontract`, NRI plugin), #893, #892 (NRI plugin) and
  #174, #906 (`protocol.go`) touch the same files.
- **Status**: fork only. A new annotation is a public API: it needs an
  approved issue, likely a SNEP.

## 2. Take the restore Pod's IP from its sandbox

Commit `81d92cfb` `feat(agent): take the restore Pod's IP from its sandbox before kubelet publishes it`
(two commits squashed: the sandbox lookup and the bounded wait within a pass).

- **What**: with `config.criu.tcpEstablished` (the default), when the API has
  no Pod IP yet the restore preflight reads the IPs of the Pod's ready sandbox
  through CRI (`ListPodSandbox` by kubelet's Pod UID label, then
  `PodSandboxStatus`) and picks the one kubelet will publish (first IP of the
  node's primary family from `status.hostIP`, else the first of the other
  family) for both the gate and the restore's `TargetPodIP`. The API's IP stays
  the first choice. A pass that finds no IP asks again every 100 ms, also
  checking the informer's newest copy of the Pod, for at most 5 s from the
  Pod's first wait, without writing conditions or events. Host-network Pods
  and vCluster tenants (whose Pod UID differs on the node) wait for the API as
  before.
- **Why**: the preflight stayed `PodIPPending` until `status.podIP` was set,
  which kubelet does only at its next PLEG relist, up to a second after the
  sandbox starts; on a node without NRI nothing requeued the Pod, so the next
  pass came with kubelet's next status update or the 30 s safety requeue. Both
  delays add directly to restore time.
- **Files**: `agent/internal/controller/{pod_ip,controller,restore_from_map}.go`,
  `agent/internal/runtime/{pod_ips,runtime,oci_containerd,oci_crio}.go`,
  `CHANGELOG.md`.
- **Tests**: `controller/pod_ip_test.go` (sandbox IP before the API, API
  preferred, no usable IP, node family, no TCP restore, restore-from-map, wait
  within the pass and its bound, API IP arriving during the wait, stop,
  host network), `runtime/pod_ips_test.go`, `controller_test.go`, and fakes
  updated in `executor/{checkpoint,restore}_test.go`.
- **Upstream**: not covered: main still reports `PodIPPending` and takes
  `TargetPodIP` from `status.podIP`; nothing upstream calls
  `ListPodSandbox`/`PodSandboxStatus`. Precedent for the same class of fix
  (do not wait for kubelet to publish what the runtime already knows):
  [#703](https://github.com/ai-dynamo/snapshot/issues/703) /
  [#704](https://github.com/ai-dynamo/snapshot/pull/704).
- **Against upstream main**: applies cleanly on top of patch 1; on its own it
  needs the `restore_from_map.go` hunk dropped (it extends patch 1 to the new
  annotation). The open restore-retry stack (#901, #906) adds its own CRI read
  in `runtime.go`, `oci_containerd.go`, `oci_crio.go`.
- **Status**: fork only.

## 3. LZ4 memory page compression and parallel decoding

Commit `c832346a` `feat(agent): compress memory pages with CRIU's LZ4 and decode them in parallel`.

- **What**: `config.criu.compress` (default `false`) and
  `config.criu.compressBlockBytes` (default `0`, CRIU's size) set CRIU's
  `compress` and `compress_block_size` on dump; `config.criu.decompressThreads`
  (default `1`; `0` lets CRIU choose) sets `decompress_threads` on restore,
  taken from the restoring agent and passed to `nsrestore`. The agent image
  builds CRIU with `liblz4-dev` and ships `liblz4-1`. At startup the agent asks
  CRIU for the `mem_compression` feature, logs it, and fails a checkpoint that
  asks for compression where CRIU cannot, before the source is touched.
  Wired through the chart values, schema and ConfigMap. Defaults leave dump
  and restore requests unchanged.
- **Why**: CRIU can store memory pages in LZ4 blocks (all-zero blocks take no
  space, incompressible blocks stay raw) and decode them on a worker pool,
  which shrinks GPU workload snapshots and speeds up restores from shared
  storage; the agent image built CRIU without LZ4, so the feature was missing.
- **Depends on**: a CRIU with memory page compression. Upstream's agent image
  builds CRIU from `criu-dev`; this fork's CI pins `CRIU_REF` to the
  `criu-dev` commit the feature was verified on.
- **Files**: `agent/Dockerfile`, `agent/cmd/{agent,nsrestore}/main.go`,
  `agent/internal/criu/{compression,dump}.go`,
  `agent/internal/executor/{checkpoint,restore,nsrestore}.go`,
  `agent/internal/controller/controller.go`, `agent/internal/types/config.go`,
  `charts/snapshot/{values.yaml,values.schema.json,templates/configmap.yaml}`,
  `docs/reference/api.md`, `CHANGELOG.md`.
- **Tests**: `criu/compression_test.go` (feature probe, dump options, restore
  threads), `executor/compression_test.go` (refuses compression CRIU cannot
  do), `controller/restore_request_test.go`, `types/config_test.go`, chart
  `tests/config_test.yaml` (defaults, rendering, schema rejections).
- **Upstream**: not covered: nothing upstream compresses CRIU images and the
  agent's CRIU build has no `liblz4`.
  [#573](https://github.com/ai-dynamo/snapshot/issues/573) (open) is about
  compressing the GPU payload in PageBroker and leaves CPU/CRIU images as they
  are, so the two complement each other.
- **Against upstream main**: four conflicts where both sides add code at the
  same place (the checkpoint retry of #818 in `executor/checkpoint.go`,
  `types/config_test.go`, `values.schema.json`) plus `docs/reference/api.md`,
  whose values table moved to `docs/reference/helm-values.md`. Open PRs #801,
  #896, #906, #912, #838 touch the same files.
- **Status**: fork only. New configuration fields need an approved issue.

## 4. Chart: `daemonset.restoreProxy.enabled`

Commit `cd86f4d3` `feat(chart): let an install keep the agent out of container creation`.

- **What**: `daemonset.restoreProxy.enabled: false` leaves the NRI socket out
  of the agent DaemonSet, so the agent restores without the restore proxy, as
  on a node without NRI (restore Pods then need an inert command). Default
  `true`, unchanged.
- **Why**: the restore proxy's NRI plugin is called by the container runtime
  for every container created on the node. On nodes that also run other
  workloads an install may not want the agent in that path.
- **Files**: `charts/snapshot/{values.yaml,templates/daemonset.yaml}`,
  `docs/reference/restore-pod-contract.md`, `CHANGELOG.md`.
- **Tests**: chart `tests/restore_proxy_test.yaml`.
- **Upstream**: not covered: upstream has `daemonset.nriHostNamespace` (#504),
  which limits which Pods get the proxy but cannot turn it off; the fallback
  this relies on, restoring without the proxy where NRI is missing, is
  upstream ([#633](https://github.com/ai-dynamo/snapshot/pull/633)). If
  [#893](https://github.com/ai-dynamo/snapshot/pull/893) (deliver cuInterpose
  through the NRI plugin, issue #834) merges, turning the NRI socket off would
  also stop cuInterpose delivery, so the value would need another name (for
  example one about the NRI plugin rather than the restore proxy).
- **Against upstream main**: applies cleanly (only `CHANGELOG.md` conflicts,
  from the patches before it).
- **Status**: fork only.

## 5. Capture a Pod's containers one after the other

Commit `efa13477` `fix(agent): let a pod's containers be captured one after the other`.

- **What**: the crash sweep of a checkpoint skips containers that another
  Ready work order (PodSnapshotContent) of the same Pod incarnation captured.
  A container whose capture failed is not skipped, so a crash still fails the
  next capture.
- **Why**: a Pod can be the source of several PodSnapshots, one container
  each. Work orders on one Pod already run one at a time, but the second one
  failed at once: the container the first dump ended had exited 137, which the
  sweep read as a crashed checkpoint container, and it then killed every
  container left running, including the one about to be captured.
- **Files**: `agent/internal/controller/podsnapshotcontent.go`, `CHANGELOG.md`.
- **Tests**: `podsnapshotcontent_test.go`
  (`TestFailCheckpointOnContainerExit_IgnoresContainerAnotherWorkOrderCaptured`,
  `..._StillFailsOnContainerWhoseCaptureFailed`).
- **Upstream**: not covered: the crash sweep (`failCheckpointOnContainerExit`,
  `failedCheckpointContainer`) is unchanged on main, although the capture retry
  work around it (#818, #822, #829, #830) was rewritten. Related context: #385
  (one capture per source Pod at a time), #822 (retry a failed capture).
- **Against upstream main**: applies cleanly on its own and on top of the
  others; its tests pass there. No open PR conflicts with it.
- **Status**: fork only. A bug fix: the easiest to take upstream.

## 6. End a restore that will not run

Commit `dfcf8c2e` `fix(agent): end a restore that will not run instead of leaving it waiting`.

- **What**: a restore the compatibility checks refuse, or whose preflight
  fails, ends its destination containers the way a failed restore does:
  `restore-failed`, then the kill. `config.restore.snapshotWaitSeconds`
  (default `60`; `0` keeps the unbounded wait) bounds the wait for a missing
  or not-Ready PodSnapshot, counted from the first destination container's
  start; past it the restore fails the same way. Containers that start later
  are ended when they start. The agent never restores into a container that
  holds `restore-failed`, and does not kill a container twice or one that
  holds `restore-complete`. Condition reasons are unchanged.
  `nvidia.com/snapshot-skip-compat-check` added to a refused Pod no longer
  reopens its restore; that takes a new Pod.
- **Why**: a refused restore only got its condition, so the destination
  container waited out its own timeout before starting cold; a restore whose
  PodSnapshot never became Ready waited indefinitely, and a workload that had
  meanwhile started without it could be restored into once the PodSnapshot
  appeared.
- **Behaviour change**: the default wait goes from unbounded to 60 s.
- **Files**: `agent/internal/controller/{restore_end,compat,controller}.go`,
  `agent/internal/types/config.go`, `api/podcontract/protocol.go` (doc comment),
  `charts/snapshot/{values.yaml,values.schema.json,templates/configmap.yaml}`,
  `e2e/tests/test_snapshot_lifecycle.py`, docs (`troubleshooting.md`, `api.md`,
  `architecture/README.md`, `restore-pod-contract.md`), `CHANGELOG.md`.
- **Tests**: `restore_end_test.go` (refusal and preflight failure end every
  destination, snapshot wait for every not-ready case and unbounded, no restore
  after giving up, late destinations, idempotent kill, restored destination
  spared), `compat_test.go`, `controller_test.go`, `types/config_test.go`,
  chart `tests/config_test.yaml`, e2e `test_snapshot_lifecycle.py` adjusted to
  the new refusal behaviour.
- **Upstream**: not covered on main: a refusal only sets the condition, and
  `nvidia.com/snapshot-skip-compat-check` reopens a refused restore (this patch
  removes that escape hatch, which reviewers will question). Open designs
  overlap: [#800](https://github.com/ai-dynamo/snapshot/pull/800) (SNEP-322,
  cold-start fallback after a failed restore; issues #244, #322, #689) ends an
  incompatible restore with a kill and asks, as an open question, whether a
  missing snapshot should fall back; `snapshotWaitSeconds` is one answer.
  [#836](https://github.com/ai-dynamo/snapshot/pull/836) (SNEP-247, retrying a
  failed restore) and its drafts #895–#906 add `restore.retryLimit` next to the
  new key (#896), a deadline per attempt (#899) and an agent that never runs
  CRIU after a written outcome (#902). Precedents: #674 (a missing artifact on
  a Ready snapshot fails the restore), #481 (`restore-failed` before every
  kill).
- **Against upstream main**: the code applies cleanly; the docs conflict with
  #786 (`docs/operations/troubleshooting.md` is now
  `docs/troubleshooting/restores.md`). Open PRs #896, #906 (`config.go`, chart
  values) and #174 (`compat.go`) touch the same code.
- **Status**: fork only. Changes restore semantics: needs an approved issue
  and agreement with the SNEP-322/SNEP-247 authors.

## 7. Own condition reasons for restores that did not run

Commit `3038962f` `feat(agent): give restores that did not run their own condition reasons`.

- **What**: two outcomes of the `nvidia.com/Restored` condition that were
  `RestoreFailed` get their own reason. `RestoreSnapshotNotReady`: the agent
  ended the restore because its PodSnapshots were not ready to restore within
  `config.restore.snapshotWaitSeconds` (patch 6); the message is unchanged
  (`Stopped waiting for the snapshot after ...`). `RestoreEndedByWorkload`:
  every destination container already held a `restore-failed` that its
  workload wrote, so the agent left it alone; the message is
  `Restore from <PodSnapshots> did not run: the workload of N destination
  container(s) started without a restore first: <destination>: <cause>; ...`.
  Both classify as `Failed` in `podcontract.ClassifyRestoreOutcome`, so the
  agent treats them as terminal. To tell its own `restore-failed` from the
  workload's, the agent now writes `owner=agent` into the file (it wrote
  `failed`, and PageBroker restores `restore failed`);
  `podcontract.RestoreFailedByAgent` reads it, line by line, skipping lines
  that are not `key=value`. A destination whose `restore-failed` is the
  agent's (a restore it failed or ended in an earlier pass or agent) stays a
  failure. A Pod whose destinations ended in different ways keeps
  `RestorePartiallySucceeded`, `RestoreIncompatible` or `RestoreFailed`, and
  the message lists every destination's cause. Other reasons are unchanged.
- **Why**: whoever counts failed restores against a snapshot (a controller
  that disables a bad snapshot after N failures) must not count a snapshot
  that was not ready yet, or a workload that gave up waiting before the agent
  began; until now only the message told them apart.
- **Files**: `api/podcontract/{protocol,restore_status}.go`,
  `agent/internal/controller/{controller,restore_end}.go`,
  `agent/internal/runtime/control.go` (`ReadControlFile`: bounded, regular
  files only, no symlinks, no blocking on a FIFO),
  `agent/internal/executor/restore.go`, docs (`troubleshooting.md`,
  `restore-pod-contract.md`, snapshotctl README), `CHANGELOG.md`.
- **Tests**: `podcontract/{protocol,restore_status}_test.go` (owner parsing,
  classification), `runtime/control_test.go` (`ReadControlFile`),
  `controller/restore_end_test.go` (snapshot wait reason, workload's marker,
  agent's marker, several destinations ending differently, verdict table). The
  e2e helpers' lists of terminal reasons (`e2e/snapshot_e2e/lifecycle.py`)
  were left as they are: they still list only the three old reasons, so an e2e
  wait sees the new ones as not terminal and times out instead of failing
  early.
- **Upstream**: not covered: upstream reports both as `RestoreFailed`
  (upstream has no snapshot wait; that is patch 6). The restore-retry drafts
  add reasons of their own around attempts (#899, #902); agree on names there.
- **Against upstream main**: builds on patch 6 (`restore_end.go`); the code
  otherwise applies where patch 6 does. `controller.go` is also touched by
  #902 and #906.
- **Status**: fork only. New condition reasons are a public API change: needs
  an approved issue, together with patch 6.

## 8. Claim a destination container before restoring into it

Commits `41161579` `fix(agent): claim a destination container before restoring into it`
and `df4f9e29` `fix(agent): create the restore claim whole and judge outcomes by it`
(the fixes from a joint review of both halves; squash them when the branch is
rebuilt).

- **What**: the agent claims each destination container, by creating
  `restore-claim` in its control directory whole (a temporary file
  hard-linked to that name, which fails if it exists), before it restores
  into it and before it ends it with `restore-failed` and a kill.
  A container whose workload claimed it first is left alone, and the restore
  reports `RestoreClaimedByWorkload`. The claim is never removed. The protocol
  is below; the workload half is the Kamin entrypoint's restore claim
  (`docs/usage.md`, section 恢复认领, in the Kamin repository).
- **Why**: on a node without the restore proxy the destination container runs
  its own command, which gives up waiting for the restore after its own
  timeout and starts the workload without one. The agent read the
  `restore-failed` such a command writes only before the restore began
  (patch 6), and the restore cleared it before CRIU (`nsrestore`), so a
  restore slower than the workload's wait ran into a container that was
  already running the workload: two engines on one GPU.
- **Files**: `api/podcontract/{protocol,restore_status}.go`,
  `agent/internal/runtime/control.go` (`ClaimControlFile`),
  `agent/internal/controller/{restore_claim,restore_end,controller}.go`,
  `agent/internal/executor/nsrestore.go` (`clearRestoreOutcome`, which keeps
  the claim), docs (`restore-pod-contract.md` section "Claiming the
  container", `troubleshooting.md`, snapshotctl README), `CHANGELOG.md`.
- **Tests**: `runtime/control_test.go` (claim created once, a second claim and
  the workload's `O_EXCL` after it fail, the workload first, symlinks not
  followed, 200 rounds of 4 agent and 4 workload racers with exactly one
  winner, missing control directory), `executor/nsrestore_test.go` (clearing
  the outcome keeps the claim), `podcontract/protocol_test.go` (format, parse,
  own claim), `controller/restore_claim_test.go` (the claim exists before the
  restore starts and the workload's claim then fails; workload claimed first:
  no restore, no kill, nothing written, `RestoreClaimedByWorkload`; agent
  restart after claiming, same container: restores; container restarted
  since: no restore, no kill, `restore-failed`, `RestoreFailed`; claims left
  by an earlier container of the Pod: the workload's, the agent's, an
  unreadable one; claim error fails the restore; ending a restore kills only
  containers the agent claimed; a refusal after the claim still ends the
  container; each destination of a restore-from-map Pod claimed on its own;
  verdict when destinations were declined in different ways; a claim read
  empty is read again; unknown owners; recovery trusts `restore-complete` only
  for the claimed container; the workload's `restore-failed` under the
  agent's claim is a failure). The runtime race test also has readers that
  never see the claim empty, and checks no temporary file is left. The test
  runtime now resolves a placeholder by default, since every restore claims
  through one; tests that need resolution to fail say so.
  `runtime/claim_workload_test.go` races the agent's claim against a real
  workload entrypoint for 300 rounds, at random moments around its deadline;
  it runs only with `KAMIN_ENTRYPOINT_BIN` set to the binary.
  `controller/restore_claim_race_test.go` (build tag `kamin_claim_race`) runs
  the agent's whole restore path against the real entrypoint; see "Joint race
  test with the entrypoint" below.
- **Upstream**: not covered; nothing upstream lets a workload and the agent
  agree on who starts a destination container. Collisions:
  [#884](https://github.com/ai-dynamo/snapshot/pull/884) (SNEP-883, restore on
  container restart) restores into a restarted container, which the permanent
  claim forbids; the claim would have to be per container there.
  [#836](https://github.com/ai-dynamo/snapshot/pull/836) (SNEP-247, retrying
  a failed restore) and its drafts (#895–#906) retry in the same Pod; a retry
  into the same container passes (the agent's own claim), one into a restarted
  container does not. [#800](https://github.com/ai-dynamo/snapshot/pull/800)
  (SNEP-322, cold-start fallback) is the upstream design for the workload
  starting without a restore; the claim is a mechanism it could adopt.
- **Against upstream main**: builds on patches 6 and 7 (`restore_end.go`,
  `restoreDeclinedError`). #902 (pick a restore up after an agent restart)
  rewrites the same part of `runRestore`.
- **Status**: fork only. A new control file and restore semantics: needs an
  approved issue, likely together with SNEP-322.

### The restore claim protocol

Both sides must follow this exactly; the Kamin entrypoint's half is checked
against it.

- **File**: `restore-claim` in the destination container's control directory,
  `$SNAPSHOT_CONTROL_DIR/restore-claim` (`/snapshot-control/restore-claim`,
  the container's `subPath` of the Pod's `snapshot-control` emptyDir). The
  agent reaches it through `/host/proc/<placeholder PID>/root`, resolving the
  path inside the container's root. It is a reserved control file
  (`podcontract.IsReservedControlFile`).
- **Claiming, created whole**: write the content into a temporary file in the
  same directory (the agent: `.restore-claim.<random>`, created with
  `O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC`, mode `0644` so a workload
  running as another user can read it; the agent reads any claim as root),
  then `linkat(2)` it to `restore-claim`, then remove the temporary file. Whoever's
  link succeeds owns the container until the Pod is deleted. `EEXIST` means
  someone else claimed it. The claim is never visible empty or cut short; a
  temporary file left behind by a crash is only litter. A failure other than
  `EEXIST` gives the claimer no claim: the agent then fails the restore
  (`restore-failed` if it can, kill); the entrypoint's handling is below.
- **Content**: `key=value` lines ending in `\n`; spaces around keys and
  values are trimmed on both sides, and unknown keys are ignored. The agent
  writes `owner=agent\ncontainer=<runtime container ID, without the CRI
  scheme>\n`; the workload writes `owner=workload\n`. `owner` must be exactly
  `agent` or `workload` after trimming; anything else, a missing `owner`
  included, makes the claim unreadable. The agent reads a claim that does not
  parse again, up to 5 times 20 ms apart, before it takes it as unreadable;
  with both sides linking, this only covers writers that create the file
  before they write it.
- **Nobody removes, renames or rewrites it**, the agent's clearing of
  `restore-complete` and `restore-failed` before CRIU included. It outlives the
  container's restarts.
- **Agent, restoring** (`runRestore`): (1) with the Pod `RestoreInProgress`
  after an agent restart and `restore-complete` present, only the post-restore
  action runs, without a new claim, and only if the claim is the agent's for
  this container (or there is no claim at all, from an agent before claims):
  a `restore-complete` left by a restore into an earlier container of the Pod
  does not make a container that started since count as restored; (2) a
  container holding `restore-failed` is refused: `RestoreFailed` if the file
  says `owner=agent` or the claim is not the workload's (the agent's for this
  or an earlier container, or unreadable), so a workload that waited out the
  agent's claim and exited 114 is never paired with a reason that does not
  count; `RestoreEndedByWorkload` if the claim is the workload's or there is
  none; (3) the claim; (4) only then `executor.Restore`: artifact metadata,
  placeholder inspection with the second compatibility check
  (`executor/restore.go`, `inspectRestore`), bundle mount into the container,
  PageBroker staging onto the GPUs, `nsrestore` (rootfs diff, clearing of
  `restore-complete`/`restore-failed`, CRIU, CUDA); (5) `restore-complete`;
  (6) the post-restore action. What the claim finds decides:

  | Claim | The agent | Outcome |
  |---|---|---|
  | created now | restores | as before |
  | `owner=agent`, `container=` this container | restores: the agent restarted after it claimed | as before |
  | `owner=workload` | nothing: no restore, no kill, no file written | `RestoreClaimedByWorkload` when every destination was claimed so, else listed in the Pod's message |
  | `owner=agent`, another container (an earlier one of this Pod) | no restore, no kill; writes `restore-failed` (`owner=agent`) unless present | `RestoreFailed` |
  | empty, no `owner`, an `owner` other than `agent`/`workload`, not a regular file, or gone | as the row above | `RestoreFailed` |
  | create fails other than `EEXIST`, or the container cannot be resolved | fails the restore: `restore-failed`, kill | `RestoreFailed` |

- **Agent, ending a restore** (refusal, failed preflight, snapshot wait;
  `endDestination`): a container holding `restore-failed` or
  `restore-complete` is left alone, as before; otherwise the agent claims it.
  Created or its own: `restore-failed`, then the kill. The workload's: nothing.
  Any other: `restore-failed` unless present, no kill. If the claim cannot be
  decided, it ends the container anyway, as it does when it cannot read
  `restore-failed`. A container with no running process gets `restore-failed`
  through the node's path of the volume, without a claim.
- **Workload** (what the agent expects; the Kamin entrypoint implements it):
  in a restore container, claim before every start without a restore. Created,
  or the claim says `owner=workload` (its own, from an earlier container of the
  Pod): write `restore-failed`, then start. Anything else, empty and unreadable
  included, is the agent's: do not start; wait for `restore-complete` (resume)
  or `restore-failed` (exit; the next container starts) up to a limit, then
  write `restore-failed` and exit (the entrypoint: 114). A container that finds
  `restore-failed` or `restore-complete` when it starts may start without
  claiming: the agent refuses `restore-failed` before it claims, and the claim
  left by the restore that wrote `restore-complete` keeps it out. If the
  create fails other than `EEXIST`, the entrypoint starts without a restore
  only on `EROFS`: nobody, root included, can create the claim on a read-only
  filesystem, so no restore can follow. On anything else, `ENOSPC`, `EDQUOT`
  and `EACCES` included, it does not start and exits 2: the agent runs as
  root, and ext4's blocks reserved for root and quotas that exempt root may
  still let it claim where the workload cannot. (The entrypoint's `8b1b2e8`
  settled this; the earlier `69f6169` also started on `ENOSPC` and `EDQUOT`.)
- **Why the agent writes `restore-failed` for a claim that is neither its own
  nor the workload's**: the workload reads such a claim as the agent's and
  waits for its outcome, and the restore proxy waits with no limit. The agent
  will not restore there, so `restore-failed` is the outcome it owes. Without
  it the entrypoint waits one more `KAMIN_RESTORE_TIMEOUT` and exits 114, and
  the proxy waits forever. When the workload holds the claim the agent writes
  nothing.
- **Timing**: the workload's first deadline (`KAMIN_RESTORE_TIMEOUT`, from its
  own start) covers the agent's snapshot wait (`snapshotWaitSeconds`, from the
  container's start) and the preflight with its first compatibility check;
  the agent claims only after those. Everything else runs after the claim:
  reading the artifact's metadata, inspecting the placeholder with the second
  compatibility check, mounting into the container, PageBroker staging and
  the restore itself. A container the second check refuses is ended at once
  (`restore-failed`, kill: `RestoreIncompatible`), so a refusal after the
  claim does not run into the workload's second wait. If the agent holds the
  claim at the first deadline, the workload waits one more
  `KAMIN_RESTORE_TIMEOUT`, so a claimed restore gets at least that long. The
  agent's own bound is `config.restore.timeoutSeconds` (default 7200 s), far
  longer: a restore that outlasts the workload's second wait is cut by the
  workload, which writes `restore-failed` and exits 114. The container's PID
  namespace ends with it, taking any restored process along, and the agent's
  restore then fails (`RestoreFailed`, counted against the snapshot). So keep
  `KAMIN_RESTORE_TIMEOUT` above the snapshot wait plus the restore's p99 plus a
  margin, and, to have the agent report a slow restore itself, keep
  `config.restore.timeoutSeconds` below the workload's second wait; that value
  applies to every restore on the node. Not measured on a GPU yet.
- **Known edge**: if the workload gives up at its limit while `nsrestore` is
  about to clear the sentinels, `nsrestore` may remove the workload's
  `restore-failed`. The workload exits anyway, so no two engines; its next
  container finds the agent's claim without an outcome and waits again until
  the agent's failed restore writes `restore-failed`, or exits 114 once more.

### Joint race test with the entrypoint

`agent/internal/controller/restore_claim_race_test.go` runs this agent's
restore path against the real Kamin entrypoint (`kamin-entrypoint run`,
restore mode), the workload half of the claim. It is opt in: build tag
`kamin_claim_race`, Linux only, `KAMIN_ENTRYPOINT_BIN` naming the binary.

- **What is real**: the entrypoint, waiting in a destination container with
  the container's control directory; the agent's `runRestore` and
  `endDestination` with their claim, read, sentinel and kill operations
  (`ClaimControlFile` and the rest, reaching the directory through
  `<proc>/<pid>/root` of the entrypoint's process, as through `/host/proc` on
  a node; the build tag adds variants that take the proc root); container
  restarts, which start the entrypoint again on the same directory with a new
  process and runtime ID. Run as root, the entrypoint and the workload run as
  user 65532 on a world-writable control directory, as a non-root workload
  does on an emptyDir.
- **What stands in**: the restore itself (it enters the container, takes as
  long as the test says, clears `restore-complete`/`restore-failed` as
  `nsrestore` does, and starts a process for the restored workload; it fails
  if the container ended meanwhile), the container runtime (a container
  resolves to its entrypoint while that runs), the API server (the fake
  clients of the other controller tests), and the PID namespace (when a
  container's entrypoint exits, the test kills the processes in it). The
  restored process is not the entrypoint's child, so the entrypoint follows it
  through `/proc` and exits 111 when it goes, where in a container it would
  exit with the workload's status.
- **Cases**: the agent first; the workload first; both at once (81 claim
  moments, 250 µs apart, around the end of the entrypoint's wait; each side
  must win at least once); the entrypoint restarted before any claim, during
  the agent's claimed restore, and after a restore, with an agent restart that
  comes back to the Pod; an agent restart during its own restore (the new
  agent finds its claim for the same container and restores); a claimed
  restore that does not finish (the entrypoint exits 114 after two restore
  timeouts and the next start starts the workload) or finishes during the
  second wait; the agent ending a restore whose snapshot never became ready.
  After each: no container both had a restore enter it and started its
  workload, and the agent killed only containers whose claim was its own.
- **Run**: from `agent/`,
  `KAMIN_ENTRYPOINT_BIN=<path>/kamin-entrypoint go test -count=1 -tags kamin_claim_race -run ClaimRace ./internal/controller/`
  (about 20 s; add `-race` for the data race detector, about 100 s).
  `KAMIN_CLAIM_RACE_DIR` puts the test's directories on a given filesystem.
- **On a GPU node** (Kamin's Z0 smoke test): build a static test binary,
  `CGO_ENABLED=0 go test -c -tags kamin_claim_race -o claim-race.test ./internal/controller/`
  from `agent/`, and take `/kamin-entrypoint` out of the exact entrypoint
  image the restore Pods use (`docker create <image>@<digest> /kamin-entrypoint`,
  then `docker cp`). Run a Pod on the GPU node itself (`nodeName`), with the
  runtime class of the restore Pods, no GPU request, `runAsUser: 0` (the test
  drops the entrypoint to user 65532), an image with `tar` for `kubectl cp`,
  and an emptyDir mounted at `/race` on the same medium as the restore Pods'
  `snapshot-control` volume. Copy both binaries to `/race`, then run
  `KAMIN_ENTRYPOINT_BIN=/race/kamin-entrypoint KAMIN_CLAIM_RACE_DIR=/race /race/claim-race.test -test.run ClaimRace -test.count 3 -test.v`.
  Record the node's kernel, the filesystem (`stat -f -c %T /race`), the
  entrypoint's version and digest, and the result. The live half of Z0, a
  real restore slower than `KAMIN_RESTORE_TIMEOUT`, then checks what this test
  stands in for: CRIU into the container's namespaces and the restored
  process ending with the container.

## 9. Restore only into a container started with the snapshot's inputs

Commit `0ff9210b` `feat(agent): restore only into a container started with the snapshot's inputs`.

- **What**: a checkpoint records a salted SHA-256 digest of every environment
  variable of the target container's runtime spec and of its arguments, in
  the manifest (`inputs`, below), and lists what it recorded in
  `PodSnapshotContent.status.source.pod.recordedInputs` (`Env`, `Args`). A
  restore Pod's `nvidia.com/restore-match-inputs` annotation, a JSON object
  keyed by destination container, names the variables (`env`) and whether the
  arguments (`args`) must equal the snapshot's. The agent compares them in
  `inspectRestore`, after it claims the destination (patch 8) and before
  anything else inspects or touches it. A difference, a snapshot without a
  record, a record of another version, an annotation it cannot read in full
  (unknown keys, duplicate keys, an entry that names nothing, a container that
  is not a destination), or `args` on a node with the restore proxy refuse the
  restore: the destination is ended like an incompatible one (`restore-failed`,
  kill) and the Pod's reason is `RestoreInputsChanged`, which classifies as
  `Failed`. Messages, events and logs name inputs, never values or digests.
  `nvidia.com/snapshot-skip-compat-check` does not skip it. A Pod without the
  annotation is restored as before.
- **Why**: Kubernetes resolves environment variables from Secrets and
  ConfigMaps, and expands `$(VAR)` in arguments, when the container starts,
  which can be minutes after the Pod was created (scheduling, image pulls). A
  restored process keeps the values it was checkpointed with. A Secret changed
  between the checkpoint, or the moment whoever created the Pod chose the
  snapshot, and the container's start gives the container the new value while
  the restored process runs with the old one. Only the node agent sees both
  what the container was started with (its runtime spec) and what the
  checkpointed container was started with, and only it runs before the
  restored process does. Kamin's webhook writes the annotation; it lists the
  variables that come from Secrets and ConfigMaps (and those built from them
  with `$(VAR)`), never the per-Pod ones.
- **The manifest record** (`agent/internal/types/inputs.go`):

  ```yaml
  inputs:
    version: 1              # the digest construction; another version is refused
    recorded: [env, args]   # kinds recorded; a later kind (files) is added here
    salt: <64 hex>          # 32 random bytes per checkpoint
    env: {<NAME>: <64 hex>} # every variable the container had
    args: <64 hex>
  ```

  Digest, version 1: SHA-256 of `snapshot-restore-inputs/v1\n`, the salt, then
  the kind, the name and each value, each prefixed with its length as a
  big-endian u64, the values preceded by their count. An environment
  variable's values are its whole `NAME=VALUE` entries in the runtime spec, in
  order; the arguments' name is empty. A variable absent from `env` was not
  set. Files from Secret and ConfigMap volumes are not recorded yet; they would
  be a new kind in `recorded`, not a new version.
- **Security**: the values are in the snapshot already (the environment block
  is in the saved process's memory), so digests beside them disclose nothing
  to whoever can read the artifact. They are not copied into any Kubernetes
  object, and the salt keeps equal values in two snapshots from having equal
  digests.
- **Files**: `api/podcontract/{restore_inputs,restore_status}.go`,
  `api/v1alpha1/checkpoint_source_types.go` (and the generated deepcopy and
  both CRD copies), `agent/internal/types/{inputs,manifest}.go`,
  `agent/internal/executor/{inputs,restore,checkpoint}.go`,
  `agent/internal/controller/{controller,compat,checkpointsource}.go`, docs
  (`restore-pod-contract.md` section "Restore only with the snapshot's
  inputs", `api.md`, `troubleshooting.md`, architecture overview, snapshotctl
  README), `CHANGELOG.md`.
- **Tests**: `podcontract/restore_inputs_test.go` (every malformed form:
  not JSON, null, arrays, trailing values, unknown and case-shifted keys,
  duplicate keys at both levels, entries that name nothing, invalid names,
  the size limit; destinations), `types/inputs_test.go` (record, salt per
  record, every way of setting a variable gets its own digest, validation,
  manifest round trip without the values, an old manifest reads as not
  recorded), `executor/inputs_test.go` (equal, changed, set on one side only,
  empty against set, duplicates, arguments, the restore proxy, no or
  unreadable record; the check runs before `nvidia-smi` and with compatibility
  checks skipped; a whole `Restore` refused for its inputs mounts nothing and
  no value or digest reaches the error or any log line at verbosity 10;
  `configureCheckpoint` records the inputs), `controller/restore_inputs_test.go`
  (through the real executor: claimed, ended with `restore-failed` and a kill,
  `RestoreInputsChanged`, one event, no value, salt or digest in the condition,
  events or logs; a snapshot without a record; a malformed annotation and a
  non-destination end the restore in preflight; each destination checks its
  own entry; no annotation, no check; the verdict beside every other outcome;
  `recordedInputs` published only for a readable record),
  `controller/checkpointsource_test.go` (each field of
  `CheckpointSourcePod`, set alone, publishes the block: the emptiness test
  goes through reflection, not a list of fields). The CPU e2e test of the
  recorded environment (`test_snapshot_lifecycle.py`) expects
  `recordedInputs` in `status.source.pod` since `f50ad234`; before, it failed
  on every run.
- **Upstream**: not covered. The annotation and the record are opt-in and
  additive, and upstream restore Pods built by `podcontract.Build` copy the
  source's environment, so they have the same hazard. The new reason would
  join patch 7's and may collide with the restore-retry stack (#899, #902).
- **Against upstream main**: builds on patches 6 to 8 (`endDestination`, the
  claim, the reasons). Applies on top of them; `inspectRestore` and the
  manifest are unchanged on upstream main.
- **Status**: fork only. Kamin writes the annotation from its webhook; roll the
  agent out before the webhook, since an agent without this patch ignores the
  annotation and restores without the check.

## 10. Pick another link remap name when the name is taken

Commit `0fc5cb7c` `fix(agent): build CRIU with a patch that picks another link remap name when the name is taken`.

- **What**: the agent image's CRIU stage applies the patches in
  `agent/criu-patches/` to `CRIU_REF`, in file name order, and fails the build
  if one does not apply. `CRIU_REF` defaults, in `agent/Dockerfile`, to the
  `criu-dev` commit the fork releases (`b64199c8`, the one the LZ4 page
  compression of patch 3 was verified on); `kamin-release.yaml` no longer
  passes its own copy, so the Dockerfile is the one place it is set. The one
  patch, `0001-files-reg-try-another-link-remap-name-when-the-name-is-taken.patch`:
  CRIU saves a mapped or open file whose name was removed while another name
  for it remains as a link remap, a hard link `link_remap.<id>` in the file's
  directory. When that name is taken (`EEXIST`), CRIU now tries
  `link_remap.<id>.1`, `.2` and so on, up to 64 more names, where it failed the
  dump before. The first name is unchanged.
- **Why**: `<id>` is counted per dump, so two dumps of near-identical process
  trees pick the same names. The containers of one Pod share `/dev/shm`
  (containerd binds the sandbox's into each container unless the Pod mounts
  its own), and a process that maps a POSIX named semaphore made by
  `sem_open(3)` maps such a file: glibc creates the semaphore under a
  temporary name, links it to its final name and removes the temporary one.
  Python's multiprocessing locks are such semaphores. The dump of the second
  of two such containers then fails with `Can't link remap to
  /dev/shm/sem.XXXXXX: File exists`, and the capture with it. The dumps need
  not overlap: a dump that kills its tasks, as the agent's do
  (`LeaveRunning=false`), keeps its link remaps, so the capture of the Pod's
  next container finds the name taken as well. Whether the two containers'
  ids collide depends on every file and mapping each process tree holds, so
  any image, model or flag change can make the next capture of such a Pod
  fail.
- **Snapshots before and after**: restore reads a link remap's name from the
  image (the remap's `RegFileEntry.name`), opens it, and removes it once the
  restore is done; it never derives the name from the id, and the patch does
  not touch restore. A dump whose first name is free writes the same images as
  before. So snapshots taken before the patch restore with it, snapshots taken
  with it restore with an agent without it (a rollback), and their keys and
  contents are otherwise unchanged. The test below restores across both
  directions.
- **Files**: `agent/criu-patches/0001-files-reg-try-another-link-remap-name-when-the-name-is-taken.patch`,
  `agent/Dockerfile`, `.github/workflows/{kamin-checks,kamin-release}.yaml`,
  `.github/kamin/criu-link-remap-{test.sh,holder.c}`, `CHANGELOG.md`.
- **Tests**: `.github/kamin/criu-link-remap-test.sh`, run as root by the
  `kamin checks` workflow. It builds CRIU at `CRIU_REF` without the patches,
  takes the patched CRIU from the agent Dockerfile's own CRIU stage, and dumps
  and restores processes that hold such a semaphore
  (`criu-link-remap-holder.c`) in the runner's `/dev/shm`: an image dumped
  without the patch restores with it; with every first name taken, the dump
  without the patch fails with `File exists` and the one with it takes
  `link_remap.<id>.1`, and that image restores with and without the patch;
  with the patch, two identical processes dumped one after the other, and
  three pairs dumped at the same time, all succeed and restore. Every restored
  process must map its own semaphore's inode, and the restore must remove its
  link remap. The workflow also runs `make test`, `make build` and gofmt on
  every pull request to a `kamin/*` branch and on `kamin/main`, which nothing
  did before.
- **Upstream**: CRIU: not covered; `create_link_remap` in `criu/files-reg.c`
  names the link after the id alone ("Any 'unique' name works here actually"),
  and the patch applies to `criu-dev` as of 2026-11-24. To send it, add a zdtm
  test (a link remap whose name is taken). Snapshot: not covered; upstream
  builds `criu-dev` and fails the same way. The Dockerfile's patch step is
  fork-only and goes once a CRIU release has the fix.
- **Status**: fork only.

## How far upstream has moved

As of 2026-11-15 upstream main is at `94b6fa2a` (2026-11-13). `v0.3.0` is on
`release/0.3`, not on main: it is `v0.3.0-rc1` plus one backported dependency
bump. Main has 18 commits that `v0.3.0` lacks, and there is no newer release
(`release/0.3` has one more commit, a Go 1.27.4 bump).

- The restore path is unchanged on main: `controller.go`, `compat.go`, the NRI
  plugin, `api/podcontract`, `agent/internal/runtime`, `executor/restore.go`,
  `nsrestore` and the DaemonSet template are identical to `v0.3.0`.
- What moved: checkpoint retries (#818 `config.checkpoint.retryLimit`, #822,
  #829, #830), the docs restructure (#786, #930), Go 1.27.4 (#917), CRD
  validation cost (#810), cuInterpose changes (#863, #867).
- Stacking all six patches on main: every code change applies (patch 3 needs
  four both-sides-added resolutions), the docs need re-homing after #786, and
  agent, api and operator then build, vet and pass their unit tests and the
  chart's unit tests (15 suites, 124 tests). `make check` and the e2e suites
  were not run there.
- Open upstream work that will collide: the restore-retry stack (SNEP-247
  #836, drafts #895–#906), the pod-group stack (#811, #833, #858, #869, #913,
  #826, #801, #934), cuInterpose through NRI (#893, #892), SNEP-322 cold-start
  fallback (#800) and SNEP-883 restore on container restart (#884).

## Suggested order for upstream pull requests

Small, self-contained fixes first; new API and changed semantics last, after
the related designs settle.

1. **Patch 5** (capture one after the other): a bug fix, applies cleanly, no
   open PR in the way. Open a bug issue, wait for `approved`, then the PR.
2. **Patch 2** (Pod IP from the sandbox): a latency fix with precedent
   (#703/#704). Send it without the `restore_from_map.go` hunk; coordinate with
   the restore-retry stack's CRI read (#901).
3. **Patch 4** (`restoreProxy` toggle): small, but agree on the name first given
   #893/#834; ask in an issue whether upstream wants "no NRI plugin" as an
   install option.
4. **Patch 3** (LZ4 compression): new configuration fields; open an
   enhancement issue citing #573 as complementary, with the CRIU requirement
   and measured snapshot sizes and restore times.
5. **Patch 6** (end a restore that will not run): comment on #800 (SNEP-322)
   and #836 (SNEP-247) first. It splits naturally into "a refusal or failed
   preflight writes `restore-failed` and ends the containers" and "bound the
   wait for a missing snapshot"; keeping `snapshot-skip-compat-check` working
   would make the first easier to accept.
6. **Patch 1** (`restore-from-map`): a new public annotation; propose it in an
   issue (likely a SNEP) next to the pod-group API (#612/#811) and the
   prefill/decode use case (#847). The other five do not depend on it once
   patch 2 drops its hunk.
7. **Patches 7 and 8** (reasons, restore claim): after patch 6, which they
   build on. Patch 7 can go with patch 6's issue. Patch 8 is a new contract
   between the agent and the workload; raise it on SNEP-322 (#800), and
   settle how it fits SNEP-883 (#884) and SNEP-247 (#836) first.
8. **Patch 9** (restore inputs): after patches 6 to 8. A new annotation, a new
   manifest section, a status field and a reason; open an enhancement issue
   with the Secret-rotation case first.

Patch 10's CRIU patch goes to [checkpoint-restore/criu](https://github.com/checkpoint-restore/criu)
on its own, with a zdtm test, whenever it is ready; nothing here depends on
its order. Once a CRIU release has it, drop the patch and, if no other is
left, the Dockerfile's patch step.

## Contributing upstream

From `CONTRIBUTING.md`, `GOVERNANCE.md`, `AGENTS.md` and `.github/` on upstream
main:

- **Issue first.** Every non-draft PR must link (`Fixes #N` or the URL) an
  **open issue labelled `approved`** by a maintainer; the "Validate Issue
  Reference" check enforces it (`.github/workflows/validate-issue-references.yaml`,
  waivable only by maintainers with `skip-issue-check`). Issues use the
  templates; blank issues are off.
- **New API or restore semantics need agreement before code** (`GOVERNANCE.md`);
  a substantial capability or public API change needs a SNEP
  (`docs/design/README.md`). Patches 1 and 6, and probably 3, fall here.
- **DCO, no CLA.** Every commit needs `Signed-off-by` (`.github/dco.yml`
  requires it even from members). All commits on this branch carry it. There
  is no CLA.
- **Conventional Commits**; PRs are squash-merged, so the PR title becomes the
  commit subject. Follow `.github/PULL_REQUEST_TEMPLATE.md` (what/why,
  `Fixes #`, how tested, `release-note` block, docs block, checklist).
- **Checks**: `check`, `build`, `test`, DCO and issue reference. Run
  `make check test build` (`make linux-build`/`make linux-test` on macOS). New
  files need SPDX headers. Non-maintainer PRs need a maintainer's
  `/ok to test <sha>` before GPU CI runs (`.github/copy-pr-bot.yaml`).
- **Docs and changelog**: one `[Unreleased]` CHANGELOG entry per user-visible
  change, docs updated on the page that owns each fact
  (`docs/development/writing-docs.md`), new names per
  `docs/development/naming.md`, tests with every behaviour change, no
  dependency bumps on the side.
- **Review**: one maintainer approval (CODEOWNERS); push follow-up commits
  rather than force-pushing; split large PRs into independent parts. The
  author certifies the DCO and must be able to explain every line; AI
  assistance need not be disclosed.
