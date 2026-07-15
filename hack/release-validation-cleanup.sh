#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

#
# Removes all resources created by release-validation-deploy.sh.
#
# Usage:
#   ./hack/release-validation-cleanup.sh
#
# Environment variables:
#   NAMESPACE        - Kubernetes namespace (default: pvc-autoscaler-system)
#   NUM_STATEFULSETS - Number of StatefulSets that were created (default: 2)
#   STS_REPLICAS     - Replicas per StatefulSet, used to derive PVC names (default: 2)
#   NUM_DEPLOYMENTS  - Number of Deployments that were created (default: 2)
#   LABEL_PREFIX     - Label/name prefix used during deploy (default: release-val)

set -o errexit
set -o nounset
set -o pipefail

_SCRIPT_DIR=$( dirname "$(readlink -f -- "${0}")" )
source "${_SCRIPT_DIR}/common.sh"

NAMESPACE="${NAMESPACE:-pvc-autoscaler-system}"
NUM_STATEFULSETS="${NUM_STATEFULSETS:-2}"
STS_REPLICAS="${STS_REPLICAS:-2}"
NUM_DEPLOYMENTS="${NUM_DEPLOYMENTS:-2}"
LABEL_PREFIX="${LABEL_PREFIX:-release-val}"

function _delete_if_exists() {
  local _resource="${1}"
  local _name="${2}"

  if kubectl get "${_resource}" "${_name}" -n "${NAMESPACE}" &>/dev/null; then
    _msg_info "Deleting ${_resource}/${_name} ..."
    kubectl delete "${_resource}" "${_name}" -n "${NAMESPACE}" --ignore-not-found
  fi
}

function _cleanup_statefulset() {
  local _index="${1}"
  local _name="${LABEL_PREFIX}-sts-${_index}"
  local _pvca_name="${LABEL_PREFIX}-pvca-sts-${_index}"

  _delete_if_exists pvca "${_pvca_name}"
  _delete_if_exists statefulset "${_name}"

  # StatefulSets do not cascade-delete their volumeClaimTemplate PVCs; remove them explicitly.
  for r in $( seq 0 $(( STS_REPLICAS - 1 )) ); do
    _delete_if_exists pvc "data-${_name}-${r}"
  done
}

function _cleanup_deployment() {
  local _index="${1}"
  local _name="${LABEL_PREFIX}-deploy-${_index}"
  local _pvc_name="${LABEL_PREFIX}-deploy-pvc-${_index}"
  local _pvca_name="${LABEL_PREFIX}-pvca-deploy-${_index}"

  _delete_if_exists pvca "${_pvca_name}"
  _delete_if_exists deployment "${_name}"
  _delete_if_exists pvc "${_pvc_name}"
}

function _main() {
  _msg_info "Starting release validation cleanup"
  _msg_info "  Namespace:        ${NAMESPACE}"
  _msg_info "  StatefulSets:     ${NUM_STATEFULSETS} (${STS_REPLICAS} replica(s) each)"
  _msg_info "  Deployments:      ${NUM_DEPLOYMENTS}"
  _msg_info "  Label prefix:     ${LABEL_PREFIX}"

  for i in $( seq 1 "${NUM_STATEFULSETS}" ); do
    _cleanup_statefulset "${i}"
  done

  for i in $( seq 1 "${NUM_DEPLOYMENTS}" ); do
    _cleanup_deployment "${i}"
  done

  _msg_info "Release validation cleanup complete"
}

_main "$@"
