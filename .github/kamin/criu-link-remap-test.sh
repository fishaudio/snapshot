#!/usr/bin/env bash
# Fork-only: checks the CRIU patch
# agent/criu-patches/0001-files-reg-try-another-link-remap-name-when-the-name-is-taken.patch
# with a real dump and restore.
#
# Usage: sudo .github/kamin/criu-link-remap-test.sh <criu without the patches> <criu with them>
#
# Each process it dumps (criu-link-remap-holder.c) maps a POSIX named semaphore
# whose temporary name glibc removed, which CRIU saves as a link remap: a hard
# link /dev/shm/link_remap.<id>, <id> counted per dump. It checks that:
#   1. an image written without the patches restores with them;
#   2. a dump that finds its link remap name taken, the failure two containers
#      sharing /dev/shm hit, fails without the patches ("File exists") and
#      succeeds with them, and that its image restores both with and without
#      them;
#   3. with the patches, two identical processes dumped one after the other,
#      and two dumped at the same time, all succeed and restore. Without them
#      the second of a pair fails when both pick the same name; that depends
#      on the ids, so it is reported, not required.
# A restored process must map the very semaphore file it was dumped with, and
# the restore must remove the link remap.
#
# It uses the machine's /dev/shm and removes every link_remap.* there when it
# ends, so run it only on a machine of its own, such as a CI runner.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <criu without the patches> <criu with them>" >&2
  exit 2
fi
if [ "$(id -u)" != 0 ]; then
  echo "run as root: CRIU dumps and restores here" >&2
  exit 2
fi
base="$(realpath "$1")"
patched="$(realpath "$2")"
here="$(cd "$(dirname "$0")" && pwd)"

shopt -s nullglob
leftover=(/dev/shm/link_remap.*)
if [ "${#leftover[@]}" -ne 0 ]; then
  echo "/dev/shm already holds link remaps: ${leftover[*]}" >&2
  exit 2
fi

work="$(mktemp -d)"
prefix="criu-link-remap-test-$$"
holder="${work}/holder"
cleanup() {
  local pidfile pid
  # Only a PID that is still a holder: a holder's PID may be another process's by now.
  for pidfile in "${work}"/*.pid; do
    pid="$(cat "${pidfile}")"
    if [ "$(readlink "/proc/${pid}/exe" 2>/dev/null)" = "${holder}" ]; then
      kill -9 "${pid}" || true
    fi
  done
  rm -f /dev/shm/link_remap.* /dev/shm/sem."${prefix}"-*
  rm -rf "${work}"
}
trap cleanup EXIT

gcc -O2 -Wall -Wextra -Werror -pthread -o "${holder}" "${here}/criu-link-remap-holder.c"
"${base}" --version | sed 's/^/base:    /'
"${patched}" --version | sed 's/^/patched: /'

failures=0
fail() {
  echo "FAIL: $*"
  failures=$((failures + 1))
}
pass() { echo "ok:   $*"; }
note() { echo "note: $*"; }
show_log() {
  echo "----- last lines of $1"
  tail -n 25 "$1" || true
  echo "-----"
}

# start: starts a holder, a child of this shell, and sets started to its name.
# Its PID is in ${work}/<name>.pid. Not to be called in a subshell: the shell
# that started a holder is the one that can reap it.
count=0
started=""
start() {
  local name pid
  count=$((count + 1))
  name="${prefix}-${count}"
  "${holder}" "/${name}" "${work}/${name}.pid" </dev/null >/dev/null 2>&1 &
  for _ in $(seq 200); do
    [ -s "${work}/${name}.pid" ] && break
    sleep 0.05
  done
  if [ ! -s "${work}/${name}.pid" ]; then
    echo "holder ${name} did not start" >&2
    exit 1
  fi
  pid="$(cat "${work}/${name}.pid")"
  # The precondition: a mapping of a removed /dev/shm/sem.* file.
  if ! grep -q ' /dev/shm/sem\.[^ ]* (deleted)$' "/proc/${pid}/maps"; then
    echo "holder ${name} maps no removed semaphore file:" >&2
    cat "/proc/${pid}/maps" >&2
    exit 1
  fi
  started="${name}"
}

# dump <criu> <name> <label>: dumps holder <name> into ${work}/<name>.<label>.
# The status is CRIU's. A successful dump kills the holder, which is reaped here
# so that its PID is free for the restore; a failed one leaves it running.
dump() {
  local criu="$1" name="$2" label="$3" pid status=0
  pid="$(cat "${work}/${name}.pid")"
  mkdir -p "${work}/${name}.${label}"
  "${criu}" dump -t "${pid}" -D "${work}/${name}.${label}" -o dump.log -v4 --link-remap || status=$?
  if [ "${status}" -eq 0 ]; then
    wait "${pid}" 2>/dev/null || true
  fi
  return "${status}"
}

# restore <criu> <name> <label>: restores holder <name> from ${work}/<name>.<label>
# and checks it.
restore() {
  local criu="$1" name="$2" label="$3" images pid inode mapped remap
  images="${work}/${name}.${label}"
  pid="$(cat "${work}/${name}.pid")"
  if ! "${criu}" restore -D "${images}" -o restore.log -v4 -d; then
    fail "${name}: restore with $(basename "${criu}") of the ${label} image"
    show_log "${images}/restore.log"
    return
  fi
  if ! kill -0 "${pid}" 2>/dev/null; then
    fail "${name}: not running after its restore"
    return
  fi
  inode="$(stat -c %i "/dev/shm/sem.${name}")"
  mapped="$(awk '$6 ~ "^/dev/shm/" {print $5}' "/proc/${pid}/maps" | sort -u)"
  if [ "${mapped}" != "${inode}" ]; then
    fail "${name}: maps inode(s) '${mapped}' after its restore, not its semaphore's ${inode}"
  fi
  remap="$(sed -n 's/.*Remapped .* -> \(.*link_remap\.[0-9.]*\)$/\1/p' "${images}/restore.log" | head -n 1)"
  if [ -z "${remap}" ]; then
    fail "${name}: the restore log names no link remap"
  elif [ -e "/${remap#/}" ]; then
    fail "${name}: the restore left its link remap /${remap#/}"
  else
    pass "${name}: restored with $(basename "${criu}") from the ${label} image, link remap /${remap#/}"
  fi
  kill -9 "${pid}"
}

ln -sf "${base}" "${work}/criu-base"
ln -sf "${patched}" "${work}/criu-patched"
base="${work}/criu-base"
patched="${work}/criu-patched"

echo "== 1. an image written without the patches restores with them"
start
h="${started}"
if dump "${base}" "${h}" base; then
  restore "${patched}" "${h}" base
else
  fail "${h}: dump without the patches, nothing in the way"
  show_log "${work}/${h}.base/dump.log"
fi

echo "== 2. a dump that finds its link remap name taken"
# Every name a dump of a holder can pick first, taken. A holder's link remap
# id is a few dozen at most; 1024 leaves room.
for i in $(seq 0 1023); do
  : >"/dev/shm/link_remap.${i}"
done
for restorer in "${patched}" "${base}"; do
  start
  h="${started}"
  if dump "${base}" "${h}" base; then
    fail "${h}: dump without the patches succeeded although every name was taken"
    continue
  fi
  if grep -q "Can't link remap to .*: File exists" "${work}/${h}.base/dump.log"; then
    pass "${h}: dump without the patches fails: Can't link remap ... File exists"
  else
    fail "${h}: dump without the patches failed, but not on the taken name"
    show_log "${work}/${h}.base/dump.log"
  fi
  if ! dump "${patched}" "${h}" patched; then
    fail "${h}: dump with the patches, its first name taken"
    show_log "${work}/${h}.patched/dump.log"
    continue
  fi
  if grep -q "Link remap name taken, trying .*/dev/shm/link_remap\.[0-9]*\.1$" "${work}/${h}.patched/dump.log"; then
    pass "${h}: dump with the patches took the next name"
  else
    fail "${h}: dump with the patches succeeded without logging the next name"
    show_log "${work}/${h}.patched/dump.log"
  fi
  restore "${restorer}" "${h}" patched
done
for i in $(seq 0 1023); do
  rm -f "/dev/shm/link_remap.${i}"
done

echo "== 3. two identical processes"
# taken_next <name> <label>: notes whether the dump took a name after its first.
taken_next() {
  if grep -q "Link remap name taken" "${work}/$1.$2/dump.log"; then
    note "$1: its first link remap name was taken (by the other one's), it took the next"
  else
    note "$1: its first link remap name was free"
  fi
}

# One after the other: the first dump kills its holder and keeps its link
# remap, so the second finds that name taken if the two counted the same id.
start
a="${started}"
start
b="${started}"
if dump "${base}" "${a}" base; then
  if dump "${base}" "${b}" base; then
    note "${b}: without the patches the second dump succeeded: the two picked different names"
    restore "${base}" "${b}" base
  elif grep -q "Can't link remap to .*: File exists" "${work}/${b}.base/dump.log"; then
    note "${b}: without the patches the second dump fails on the first one's link remap (File exists)"
    kill -9 "$(cat "${work}/${b}.pid")"
    wait "$(cat "${work}/${b}.pid")" 2>/dev/null || true
  else
    fail "${b}: dump without the patches failed on something else"
    show_log "${work}/${b}.base/dump.log"
  fi
  restore "${base}" "${a}" base
else
  fail "${a}: dump without the patches, nothing in the way"
  show_log "${work}/${a}.base/dump.log"
fi

start
a="${started}"
start
b="${started}"
dumped=()
for h in "${a}" "${b}"; do
  if dump "${patched}" "${h}" patched; then
    taken_next "${h}" patched
    dumped+=("${h}")
  else
    fail "${h}: dump with the patches, one after the other"
    show_log "${work}/${h}.patched/dump.log"
  fi
done
for h in "${dumped[@]}"; do
  restore "${patched}" "${h}" patched
done

# At the same time, as the dumps of two containers of one Pod.
for round in 1 2 3; do
  start
  a="${started}"
  start
  b="${started}"
  dump "${patched}" "${a}" patched &
  dump_a=$!
  dump "${patched}" "${b}" patched &
  dump_b=$!
  status_a=0
  status_b=0
  wait "${dump_a}" || status_a=$?
  wait "${dump_b}" || status_b=$?
  # The dumps ran in subshells, which cannot reap the holders.
  wait "$(cat "${work}/${a}.pid")" 2>/dev/null || true
  wait "$(cat "${work}/${b}.pid")" 2>/dev/null || true
  for pair in "${a}:${status_a}" "${b}:${status_b}"; do
    h="${pair%%:*}"
    if [ "${pair##*:}" -ne 0 ]; then
      fail "${h}: dump with the patches, at the same time as another (round ${round})"
      show_log "${work}/${h}.patched/dump.log"
      continue
    fi
    taken_next "${h}" patched
    restore "${patched}" "${h}" patched
  done
done

echo
if [ "${failures}" -ne 0 ]; then
  echo "${failures} check(s) failed"
  exit 1
fi
echo "all checks passed"
