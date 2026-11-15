# Patches carried by this fork

`kamin/main` is upstream [ai-dynamo/snapshot](https://github.com/ai-dynamo/snapshot)
`v0.3.0` plus the six patches below, one commit each, in this order, then one
`ci(kamin)` commit (`.github/workflows/kamin-release.yaml`,
`.github/kamin/package-chart.sh`) and this file. Nothing else differs from
upstream. Every patch is meant to go upstream; the fork exists until they are
merged there.

Status as of 2026-11-15.

| # | Commit | Change | Upstream |
|---|--------|--------|----------|
| 1 | `c297601a` | restore one Pod's containers from several PodSnapshots (`nvidia.com/restore-from-map`) | not covered; overlaps the open pod-group API design (#612, #811) |
| 2 | `81d92cfb` | take the restore Pod's IP from its CRI sandbox before kubelet publishes it | not covered |
| 3 | `c832346a` | CRIU LZ4 memory page compression and parallel decoding | not covered; complements #573 |
| 4 | `cd86f4d3` | chart value `daemonset.restoreProxy.enabled` | not covered; naming at risk from #893 |
| 5 | `efa13477` | capture a Pod's containers one after the other | not covered |
| 6 | `dfcf8c2e` | end a restore that will not run (refusal, `config.restore.snapshotWaitSeconds`) | not covered; overlaps open designs #800 (SNEP-322), #836 (SNEP-247) |

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
