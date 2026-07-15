#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

#
# Deploys N StatefulSets and N Deployments (each with PVC-mounted pods) and
# corresponding PersistentVolumeClaimAutoscalers for release validation.
#
# Usage:
#   ./hack/release-validation-deploy.sh
#
# Environment variables:
#   NAMESPACE           - Kubernetes namespace (default: pvc-autoscaler-system)
#   NUM_STATEFULSETS    - Number of StatefulSets to create (default: 2)
#   STS_REPLICAS        - Replicas per StatefulSet (default: 2)
#   NUM_DEPLOYMENTS     - Number of Deployments to create (default: 2)
#   DEPLOY_REPLICAS     - Replicas per Deployment (default: 2)
#   STORAGE_CLASS       - StorageClass for PVCs (default: standard)
#   PVC_INITIAL_SIZE    - Initial PVC size (default: 1Gi)
#   PVC_MAX_CAPACITY    - PVCA maxCapacity (default: 10Gi)
#   AUTOSCALER_NAME     - autoscalerName field on PVCAs for sharding (default: "")
#   LABEL_PREFIX        - Label/name prefix for all created resources (default: release-val)

set -o errexit
set -o nounset
set -o pipefail

_SCRIPT_DIR=$( dirname "$(readlink -f -- "${0}")" )
source "${_SCRIPT_DIR}/common.sh"

NAMESPACE="${NAMESPACE:-pvc-autoscaler-system}"
NUM_STATEFULSETS="${NUM_STATEFULSETS:-2}"
STS_REPLICAS="${STS_REPLICAS:-2}"
NUM_DEPLOYMENTS="${NUM_DEPLOYMENTS:-2}"
DEPLOY_REPLICAS="${DEPLOY_REPLICAS:-2}"
STORAGE_CLASS="${STORAGE_CLASS:-standard}"
PVC_INITIAL_SIZE="${PVC_INITIAL_SIZE:-1Gi}"
PVC_MAX_CAPACITY="${PVC_MAX_CAPACITY:-10Gi}"
AUTOSCALER_NAME="${AUTOSCALER_NAME:-}"
LABEL_PREFIX="${LABEL_PREFIX:-release-val}"

function _deploy_statefulset() {
  local _index="${1}"
  local _name="${LABEL_PREFIX}-sts-${_index}"
  local _pvca_name="${LABEL_PREFIX}-pvca-sts-${_index}"

  _msg_info "Deploying StatefulSet ${_name} with ${STS_REPLICAS} replica(s) ..."

  kubectl apply -n "${NAMESPACE}" -f - <<EOF
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: ${_name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: ${_name}
    app.kubernetes.io/part-of: ${LABEL_PREFIX}
spec:
  replicas: ${STS_REPLICAS}
  selector:
    matchLabels:
      app: ${_name}
  template:
    metadata:
      labels:
        app: ${_name}
        app.kubernetes.io/part-of: ${LABEL_PREFIX}
    spec:
      containers:
      - name: app
        image: busybox:1.36
        command: ["sh", "-c", "while true; do sleep 3600; done"]
        volumeMounts:
        - name: data
          mountPath: /data
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: ${STORAGE_CLASS}
      resources:
        requests:
          storage: ${PVC_INITIAL_SIZE}
EOF

  _msg_info "Deploying PVCA ${_pvca_name} targeting StatefulSet ${_name} ..."

  kubectl apply -n "${NAMESPACE}" -f - <<EOF
---
apiVersion: autoscaling.gardener.cloud/v1alpha1
kind: PersistentVolumeClaimAutoscaler
metadata:
  name: ${_pvca_name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: ${LABEL_PREFIX}
spec:
  autoscalerName: ${AUTOSCALER_NAME}
  targetRef:
    apiVersion: apps/v1
    kind: StatefulSet
    name: ${_name}
  volumePolicies:
  - maxCapacity: ${PVC_MAX_CAPACITY}
    scaleUp:
      utilizationThresholdPercent: 80
      stepPercent: 10
      minStepAbsolute: 1Gi
EOF
}

function _deploy_deployment() {
  local _index="${1}"
  local _name="${LABEL_PREFIX}-deploy-${_index}"
  local _pvc_name="${LABEL_PREFIX}-deploy-pvc-${_index}"
  local _pvca_name="${LABEL_PREFIX}-pvca-deploy-${_index}"

  _msg_info "Deploying PVC ${_pvc_name} for Deployment ${_name} ..."

  kubectl apply -n "${NAMESPACE}" -f - <<EOF
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${_pvc_name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: ${_name}
    app.kubernetes.io/part-of: ${LABEL_PREFIX}
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: ${PVC_INITIAL_SIZE}
EOF

  _msg_info "Deploying Deployment ${_name} with ${DEPLOY_REPLICAS} replica(s) ..."

  kubectl apply -n "${NAMESPACE}" -f - <<EOF
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${_name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: ${_name}
    app.kubernetes.io/part-of: ${LABEL_PREFIX}
spec:
  replicas: ${DEPLOY_REPLICAS}
  selector:
    matchLabels:
      app: ${_name}
  template:
    metadata:
      labels:
        app: ${_name}
        app.kubernetes.io/part-of: ${LABEL_PREFIX}
    spec:
      containers:
      - name: app
        image: busybox:1.36
        command: ["sh", "-c", "while true; do sleep 3600; done"]
        volumeMounts:
        - name: data
          mountPath: /data
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: ${_pvc_name}
EOF

  _msg_info "Deploying PVCA ${_pvca_name} targeting Deployment ${_name} ..."

  kubectl apply -n "${NAMESPACE}" -f - <<EOF
---
apiVersion: autoscaling.gardener.cloud/v1alpha1
kind: PersistentVolumeClaimAutoscaler
metadata:
  name: ${_pvca_name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: ${LABEL_PREFIX}
spec:
  autoscalerName: ${AUTOSCALER_NAME}
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: ${_name}
  volumePolicies:
  - maxCapacity: ${PVC_MAX_CAPACITY}
    scaleUp:
      utilizationThresholdPercent: 80
      stepPercent: 10
      minStepAbsolute: 1Gi
EOF
}

function _main() {
  _msg_info "Starting release validation deployment"
  _msg_info "  Namespace:        ${NAMESPACE}"
  _msg_info "  StatefulSets:     ${NUM_STATEFULSETS} x ${STS_REPLICAS} replica(s)"
  _msg_info "  Deployments:      ${NUM_DEPLOYMENTS} x ${DEPLOY_REPLICAS} replica(s)"
  _msg_info "  StorageClass:     ${STORAGE_CLASS}"
  _msg_info "  Initial PVC size: ${PVC_INITIAL_SIZE}"
  _msg_info "  Max PVC capacity: ${PVC_MAX_CAPACITY}"
  _msg_info "  autoscalerName:   '${AUTOSCALER_NAME}'"
  _msg_info "  Label prefix:     ${LABEL_PREFIX}"

  for i in $( seq 1 "${NUM_STATEFULSETS}" ); do
    _deploy_statefulset "${i}"
  done

  for i in $( seq 1 "${NUM_DEPLOYMENTS}" ); do
    _deploy_deployment "${i}"
  done

  _msg_info "Release validation resources deployed successfully"
  _msg_info "Run 'kubectl get pvca,sts,deploy,pvc -n ${NAMESPACE} -l app.kubernetes.io/part-of=${LABEL_PREFIX}' to inspect them"
}

_main "$@"
