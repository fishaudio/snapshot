#!/usr/bin/env bash
# Fork-only: package charts/snapshot for a fork release.
#
# Usage: .github/kamin/package-chart.sh <version> <registry> <source-url> <out-dir>
#   version     the release tag, v<upstream>-kamin.<n>, e.g. v0.3.0-kamin.1
#   registry    where the release images are, e.g. ghcr.io/<owner>/snapshot
#   source-url  the repository the chart is built from, recorded as its source
#   out-dir     where snapshot-<version without v>.tgz is written
#
# The chart in git stays as upstream ships it. A staged copy gets:
#   - version <version without v> and appVersion <version>, so the empty image
#     tags in values.yaml resolve to <version>, the tag the images carry;
#   - every upstream image repository replaced by <registry>, in values.yaml
#     and in the README and unit tests that quote the defaults;
#   - <source-url> as its only source, which helm push records as the OCI
#     source annotation.
# It then lints and packages the copy, renders the package with default values
# and fails unless every Snapshot image it references is <registry>/<image>:<version>.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  sed -n '4,8p' "$0" >&2
  exit 2
fi
version=$1 registry=${2%/} source_url=$3 out=$4

if ! [[ "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-kamin\.[0-9]+$ ]]; then
  echo "version ${version} is not v<major>.<minor>.<patch>-kamin.<n>" >&2
  exit 1
fi
chart_version=${version#v}
upstream=ghcr.io/ai-dynamo/snapshot

root=$(git rev-parse --show-toplevel)
stage=$(mktemp -d)
trap 'rm -rf "${stage}"' EXIT

# -L: the chart's LICENSE is a symlink to the repository's.
cp -RL "${root}/charts/snapshot" "${stage}/snapshot"
chart=${stage}/snapshot

files=$(grep -rlF "${upstream}/" "${chart}" || true)
if [ -z "${files}" ]; then
  echo "the chart no longer names ${upstream}; update this script" >&2
  exit 1
fi
while IFS= read -r f; do
  sed "s#${upstream}/#${registry}/#g" "${f}" > "${f}.new"
  mv "${f}.new" "${f}"
done <<< "${files}"
if grep -rnF "${upstream}" "${chart}"; then
  echo "upstream registry still named in the staged chart" >&2
  exit 1
fi
for image in operator agent pagebroker; do
  grep -qE "^ +repository: ${registry}/${image}\$" "${chart}/values.yaml" || {
    echo "values.yaml does not default ${image} to ${registry}/${image}" >&2
    exit 1
  }
done

if grep -qE '^sources:' "${chart}/Chart.yaml"; then
  echo "Chart.yaml already lists sources; update this script" >&2
  exit 1
fi
printf 'sources:\n  - %s\n' "${source_url}" >> "${chart}/Chart.yaml"

helm lint "${chart}"
mkdir -p "${out}"
helm package "${chart}" --version "${chart_version}" --app-version "${version}" --destination "${out}"
package=${out}/snapshot-${chart_version}.tgz

# Every Snapshot image the chart can reference: operator, agent (DaemonSet and
# the operator's CuInterpose installer argument) and PageBroker.
rendered=$(helm template snapshot "${package}")
images=$(printf '%s\n' "${rendered}" \
  | grep -oE "(${registry//./\\.}|${upstream//./\\.})/[a-z]+:[^\" ]+" | sort -u)
expected=$(printf '%s\n' "${registry}/agent:${version}" "${registry}/operator:${version}" \
  "${registry}/pagebroker:${version}" | sort -u)
if [ "${images}" != "${expected}" ]; then
  printf 'rendered images:\n%s\nexpected:\n%s\n' "${images}" "${expected}" >&2
  exit 1
fi
helm show chart "${package}" | grep -E '^(name|version|appVersion):'
printf '%s\n' "${images}"
echo "${package}"
