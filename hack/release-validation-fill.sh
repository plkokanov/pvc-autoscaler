#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

#
# Fills the volume mounted inside a pod to a target utilization percentage,
# waits until the autoscaler resizes the backing PVC, then repeats — triggering
# NUM_ROUNDS resize cycles on a single PVC.
#
# The script execs into the existing pod container (same approach as
# consume-pod-space.sh) so the container must have sh and dd available.
# busybox images (as deployed by release-validation-deploy.sh) satisfy this.
#
# Usage:
#   POD=<pod-name> PVC=<pvc-name> ./hack/release-validation-fill.sh
#
# Environment variables:
#   POD                  - Pod name to exec into (required)
#   PVC                  - PVC name to watch for resizes (required)
#   NAMESPACE            - Kubernetes namespace (default: pvc-autoscaler-system)
#   CONTAINER            - Container name inside the pod (default: app)
#   MOUNT_PATH           - Mount path of the volume inside the container (default: /data)
#   TARGET_UTILIZATION   - Percentage of PVC capacity to fill to trigger resize (default: 85)
#   NUM_ROUNDS           - Number of resize rounds to drive (default: 3)
#   RESIZE_POLL_SEC      - Seconds between PVC capacity polls while waiting (default: 15)
#   RESIZE_TIMEOUT_SEC   - Total seconds to wait for a resize before giving up (default: 600)

set -o errexit
set -o nounset
set -o pipefail

_SCRIPT_DIR=$( dirname "$(readlink -f -- "${0}")" )
source "${_SCRIPT_DIR}/common.sh"

POD="${POD:-}"
PVC="${PVC:-}"
NAMESPACE="${NAMESPACE:-pvc-autoscaler-system}"
CONTAINER="${CONTAINER:-app}"
MOUNT_PATH="${MOUNT_PATH:-/data}"
TARGET_UTILIZATION="${TARGET_UTILIZATION:-85}"
NUM_ROUNDS="${NUM_ROUNDS:-3}"
RESIZE_POLL_SEC="${RESIZE_POLL_SEC:-15}"
RESIZE_TIMEOUT_SEC="${RESIZE_TIMEOUT_SEC:-600}"

function _usage() {
  echo "Usage: POD=<pod-name> PVC=<pvc-name> $0"
  echo ""
  echo "Required:"
  echo "  POD  - name of the pod to exec into"
  echo "  PVC  - name of the PVC to watch for capacity changes"
  echo ""
  echo "Optional env vars: NAMESPACE CONTAINER MOUNT_PATH TARGET_UTILIZATION"
  echo "                   NUM_ROUNDS RESIZE_POLL_SEC RESIZE_TIMEOUT_SEC"
}

# Converts a Kubernetes quantity string (e.g. 1Gi, 512Mi, 2000000000) to bytes.
function _quantity_to_bytes() {
  local _q="${1}"
  echo "${_q}" | awk '
    /^[0-9]+Gi$/ { sub(/Gi$/, ""); print $0 * 1073741824; exit }
    /^[0-9]+Mi$/ { sub(/Mi$/, ""); print $0 * 1048576;    exit }
    /^[0-9]+Ki$/ { sub(/Ki$/, ""); print $0 * 1024;       exit }
    /^[0-9]+G$/  { sub(/G$/,  ""); print $0 * 1000000000; exit }
    /^[0-9]+M$/  { sub(/M$/,  ""); print $0 * 1000000;    exit }
    /^[0-9]+K$/  { sub(/K$/,  ""); print $0 * 1000;       exit }
    /^[0-9]+$/   { print $0 + 0;                           exit }
    { print "0" }
  '
}

# Returns the current capacity of the PVC in bytes (from status.capacity.storage).
function _pvc_capacity_bytes() {
  local _raw
  _raw=$( kubectl get pvc "${PVC}" -n "${NAMESPACE}" \
            -o jsonpath='{.status.capacity.storage}' )
  _quantity_to_bytes "${_raw}"
}

# Fills the volume inside the pod so that used space reaches TARGET_UTILIZATION%
# of the current PVC capacity.  Writes in 64 MiB blocks via dd.
#
# $1: current PVC capacity in bytes
function _fill_to_target() {
  local _capacity_bytes="${1}"
  local _target_bytes
  _target_bytes=$( echo "${_capacity_bytes} ${TARGET_UTILIZATION}" | \
                   awk '{ printf "%d\n", $1 * ($2 / 100) }' )

  # How many bytes are already used on the mount?
  local _used_bytes
  _used_bytes=$( kubectl exec -n "${NAMESPACE}" "${POD}" \
                   -c "${CONTAINER}" -- \
                   sh -c "df -k ${MOUNT_PATH} | awk 'NR==2{print \$3 * 1024}'" )

  local _to_write=$(( _target_bytes - _used_bytes ))

  if [[ ${_to_write} -le 0 ]]; then
    _msg_info "Volume is already at or above ${TARGET_UTILIZATION}% — no fill needed this round"
    return
  fi

  _msg_info "  PVC capacity : $( echo "${_capacity_bytes}" | awk '{printf "%.1f GiB\n", $1/1073741824}' )"
  _msg_info "  Currently used: $( echo "${_used_bytes}" | awk '{printf "%.1f MiB\n", $1/1048576}' )"
  _msg_info "  Target used ($TARGET_UTILIZATION%%): $( echo "${_target_bytes}" | awk '{printf "%.1f MiB\n", $1/1048576}' )"
  _msg_info "  Writing: $( echo "${_to_write}" | awk '{printf "%.1f MiB\n", $1/1048576}' )"

  # Write in 64 MiB blocks so progress is visible and dd stays within shell limits.
  local _block_size=$(( 64 * 1024 * 1024 ))
  local _full_blocks=$(( _to_write / _block_size ))
  local _remainder=$(( _to_write % _block_size ))

  local _fill_script
  _fill_script=$( cat <<EOF
set -e
i=1
while [ "\${i}" -le "${_full_blocks}" ]; do
  _file=\$( mktemp -u -p "${MOUNT_PATH}" )
  dd if=/dev/zero of="\${_file}" bs=${_block_size} count=1 > /dev/null 2>&1
  i=\$(( i + 1 ))
done
if [ "${_remainder}" -gt 0 ]; then
  _file=\$( mktemp -u -p "${MOUNT_PATH}" )
  dd if=/dev/zero of="\${_file}" bs=${_remainder} count=1 > /dev/null 2>&1
fi
EOF
)

  kubectl exec -n "${NAMESPACE}" "${POD}" -c "${CONTAINER}" -- sh -c "${_fill_script}"
}

# Waits until the PVC capacity grows beyond the given baseline.
#
# $1: baseline capacity in bytes (the value before the expected resize)
function _wait_for_resize() {
  local _baseline_bytes="${1}"
  local _elapsed=0

  _msg_info "Waiting for PVC ${PVC} to be resized (current capacity: $( echo "${_baseline_bytes}" | awk '{printf "%.1f GiB\n", $1/1073741824}' )) ..."

  while true; do
    local _current_bytes
    _current_bytes=$( _pvc_capacity_bytes )

    if [[ ${_current_bytes} -gt ${_baseline_bytes} ]]; then
      _msg_info "PVC ${PVC} resized: $( echo "${_baseline_bytes}" | awk '{printf "%.1f GiB", $1/1073741824}' ) -> $( echo "${_current_bytes}" | awk '{printf "%.1f GiB\n", $1/1073741824}' )"
      return 0
    fi

    if [[ ${_elapsed} -ge ${RESIZE_TIMEOUT_SEC} ]]; then
      _msg_error "Timed out after ${RESIZE_TIMEOUT_SEC}s waiting for PVC ${PVC} to be resized" 1
    fi

    _msg_info "  [${_elapsed}s / ${RESIZE_TIMEOUT_SEC}s] capacity still $( echo "${_current_bytes}" | awk '{printf "%.1f GiB", $1/1073741824}' ), retrying in ${RESIZE_POLL_SEC}s ..."
    sleep "${RESIZE_POLL_SEC}"
    _elapsed=$(( _elapsed + RESIZE_POLL_SEC ))
  done
}

function _main() {
  if [[ -z "${POD}" || -z "${PVC}" ]]; then
    _msg_error "POD and PVC must be set" 0
    _usage
    exit 1
  fi

  _msg_info "Starting release validation fill"
  _msg_info "  Pod:              ${NAMESPACE}/${POD} (container: ${CONTAINER})"
  _msg_info "  PVC:              ${NAMESPACE}/${PVC}"
  _msg_info "  Mount path:       ${MOUNT_PATH}"
  _msg_info "  Target utilization: ${TARGET_UTILIZATION}%"
  _msg_info "  Rounds:           ${NUM_ROUNDS}"

  for round in $( seq 1 "${NUM_ROUNDS}" ); do
    _msg_info "--- Round ${round}/${NUM_ROUNDS} ---"

    local _capacity_before
    _capacity_before=$( _pvc_capacity_bytes )

    _fill_to_target "${_capacity_before}"
    _wait_for_resize "${_capacity_before}"
  done

  local _final_bytes
  _final_bytes=$( _pvc_capacity_bytes )
  _msg_info "Done. PVC ${PVC} final capacity: $( echo "${_final_bytes}" | awk '{printf "%.1f GiB\n", $1/1073741824}' ) after ${NUM_ROUNDS} resize round(s)"
}

_main "$@"
