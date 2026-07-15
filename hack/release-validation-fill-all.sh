#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

#
# Drives NUM_ROUNDS resize cycles on every PVC belonging to the StatefulSets
# and Deployments created by release-validation-deploy.sh.
#
# Iterates over all workloads serially (one pod/PVC at a time) so the script
# can run as a single foreground process without background job management.
# Each fill+wait cycle can take up to the autoscaler interval (default 5m)
# plus the EBS resize time, so budget ~15 min per round per PVC.
#
# Usage:
#   ./hack/release-validation-fill-all.sh
#
# Environment variables (shared with release-validation-deploy.sh):
#   NAMESPACE           - Kubernetes namespace (default: pvc-autoscaler-system)
#   NUM_STATEFULSETS    - Number of StatefulSets that were created (default: 2)
#   NUM_DEPLOYMENTS     - Number of Deployments that were created (default: 2)
#   LABEL_PREFIX        - Name prefix used during deploy (default: release-val)
#   CONTAINER           - Container name inside pods (default: app)
#   MOUNT_PATH          - Volume mount path inside containers (default: /data)
#   TARGET_UTILIZATION  - Percentage of PVC capacity to fill per round (default: 85)
#   NUM_ROUNDS          - Resize rounds to drive per PVC (default: 3)
#   RESIZE_POLL_SEC     - Seconds between capacity polls (default: 15)
#   RESIZE_TIMEOUT_SEC  - Max seconds to wait for one resize (default: 900)

set -o errexit
set -o nounset
set -o pipefail

_SCRIPT_DIR=$( dirname "$(readlink -f -- "${0}")" )
source "${_SCRIPT_DIR}/common.sh"

NAMESPACE="${NAMESPACE:-pvc-autoscaler-system}"
NUM_STATEFULSETS="${NUM_STATEFULSETS:-2}"
NUM_DEPLOYMENTS="${NUM_DEPLOYMENTS:-2}"
LABEL_PREFIX="${LABEL_PREFIX:-release-val}"
CONTAINER="${CONTAINER:-app}"
MOUNT_PATH="${MOUNT_PATH:-/data}"
TARGET_UTILIZATION="${TARGET_UTILIZATION:-85}"
NUM_ROUNDS="${NUM_ROUNDS:-3}"
RESIZE_POLL_SEC="${RESIZE_POLL_SEC:-15}"
# Default timeout is 900s: autoscaler interval (5m) + cooldown + EBS resize time
RESIZE_TIMEOUT_SEC="${RESIZE_TIMEOUT_SEC:-900}"

function _fill_pvc() {
  local _pod="${1}"
  local _pvc="${2}"

  _msg_info "Filling PVC ${_pvc} via pod ${_pod} (${NUM_ROUNDS} round(s)) ..."

  POD="${_pod}" \
  PVC="${_pvc}" \
  NAMESPACE="${NAMESPACE}" \
  CONTAINER="${CONTAINER}" \
  MOUNT_PATH="${MOUNT_PATH}" \
  TARGET_UTILIZATION="${TARGET_UTILIZATION}" \
  NUM_ROUNDS="${NUM_ROUNDS}" \
  RESIZE_POLL_SEC="${RESIZE_POLL_SEC}" \
  RESIZE_TIMEOUT_SEC="${RESIZE_TIMEOUT_SEC}" \
    "${_SCRIPT_DIR}/release-validation-fill.sh"
}

function _pod_for_statefulset() {
  local _sts_name="${1}"
  # Use ordinal 0 — always exists and has the volumeClaimTemplate PVC
  echo "${_sts_name}-0"
}

function _pod_for_deployment() {
  local _deploy_name="${1}"
  # Pick the first Running pod for this Deployment
  kubectl get pods -n "${NAMESPACE}" \
    -l "app=${_deploy_name}" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}'
}

function _main() {
  _msg_info "Starting release validation fill (all workloads)"
  _msg_info "  Namespace:          ${NAMESPACE}"
  _msg_info "  StatefulSets:       ${NUM_STATEFULSETS}"
  _msg_info "  Deployments:        ${NUM_DEPLOYMENTS}"
  _msg_info "  Target utilization: ${TARGET_UTILIZATION}%"
  _msg_info "  Rounds per PVC:     ${NUM_ROUNDS}"
  _msg_info "  Resize timeout:     ${RESIZE_TIMEOUT_SEC}s"

  for i in $( seq 1 "${NUM_STATEFULSETS}" ); do
    local _sts_name="${LABEL_PREFIX}-sts-${i}"
    local _pod
    _pod=$( _pod_for_statefulset "${_sts_name}" )
    local _pvc="data-${_sts_name}-0"
    _fill_pvc "${_pod}" "${_pvc}"
  done

  for i in $( seq 1 "${NUM_DEPLOYMENTS}" ); do
    local _deploy_name="${LABEL_PREFIX}-deploy-${i}"
    local _pod
    _pod=$( _pod_for_deployment "${_deploy_name}" )
    if [[ -z "${_pod}" ]]; then
      _msg_error "No Running pod found for Deployment ${_deploy_name}" 1
    fi
    local _pvc="${LABEL_PREFIX}-deploy-pvc-${i}"
    _fill_pvc "${_pod}" "${_pvc}"
  done

  _msg_info "All PVCs filled through ${NUM_ROUNDS} resize round(s)"
}

_main "$@"
