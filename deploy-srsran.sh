#!/bin/bash
# srsRAN Nephio Deployment Script
# Deploys CU-CP, CU-UP, and DU as separate pods via the Nephio Porch pipeline.
# Modelled on deploy-free5gc-single-vm.sh.
#
# Prerequisites:
#   - Nephio management cluster is running and kubectl is configured to it.
#   - porchctl is installed and authenticated.
#   - The srsran-operator is running in the workload cluster.
#   - The srsran blueprint has been pushed to the catalog repository.
#
# Usage:
#   ./deploy-srsran.sh
#   CLUSTER_NAME=edge1 SITE_TYPE=edge ./deploy-srsran.sh

set -e

echo "=========================================="
echo "  srsRAN Nephio Deployment Script"
echo "=========================================="

# ── Configuration ──────────────────────────────────────────────────────────
CLUSTER_NAME="${CLUSTER_NAME:-regional}"
WORKER_NODE="${WORKER_NODE:-${CLUSTER_NAME}-control-plane}"
KUBECONFIG_FILE="${KUBECONFIG_FILE:-regional.kubeconfig}"
SITE_TYPE="${SITE_TYPE:-ran}"          # WorkloadCluster label value
SRSRAN_REPO="${SRSRAN_REPO:-catalog-workloads-srsran}"
SRSRAN_PKG="${SRSRAN_PKG:-pkg-srsran}"
BLUEPRINT_WORKSPACE="${BLUEPRINT_WORKSPACE:-main}"

echo "Configuration:"
echo "  Cluster Name   : ${CLUSTER_NAME}"
echo "  Worker Node    : ${WORKER_NODE}"
echo "  Kubeconfig     : ${KUBECONFIG_FILE}"
echo "  Site Type      : ${SITE_TYPE}"
echo "  Blueprint Repo : ${SRSRAN_REPO}/${SRSRAN_PKG}@${BLUEPRINT_WORKSPACE}"
echo ""

# ── Step 1: VLAN interfaces ─────────────────────────────────────────────────
echo "=== Step 1: Create VLAN interfaces in worker node ==="
echo "  Creating dummy eth1 + VLANs 2-6 (N2/N3/E1/F1C/F1U) ..."

sudo docker exec "${WORKER_NODE}" ip link add eth1 type dummy 2>/dev/null \
  || echo "  eth1 already exists"
sudo docker exec "${WORKER_NODE}" ip link set eth1 up

declare -A VLAN_NAMES=([2]="n2" [3]="n3" [4]="e1" [5]="f1c" [6]="f1u")
for vlan_id in 2 3 4 5 6; do
  iface="eth1.${vlan_id}"
  echo "  Creating ${iface} (${VLAN_NAMES[$vlan_id]}) ..."
  sudo docker exec "${WORKER_NODE}" \
    ip link add link eth1 name "${iface}" type vlan id "${vlan_id}" 2>/dev/null \
    || echo "    ${iface} already exists"
  sudo docker exec "${WORKER_NODE}" ip link set up "${iface}"
done

echo "✓ VLAN interfaces ready"
sudo docker exec "${WORKER_NODE}" ip link show | grep -E "eth1[\.|:]" || true

# ── Step 2: RawTopology ─────────────────────────────────────────────────────
echo ""
echo "=== Step 2: Create/update RawTopology ==="

cat > /tmp/srsran-rawtopo.yaml <<EOF
apiVersion: topo.nephio.org/v1alpha1
kind: RawTopology
metadata:
  name: nephio
spec:
  nodes:
    ${CLUSTER_NAME}:
      provider: docker.io
      labels:
        nephio.org/cluster-name: ${CLUSTER_NAME}
        nephio.org/site-type: ${SITE_TYPE}
  links: []
EOF

kubectl apply -f /tmp/srsran-rawtopo.yaml
echo "✓ RawTopology applied"

# ── Step 3: NetworkInstance prefix patching ──────────────────────────────────
echo ""
echo "=== Step 3: Patch NetworkInstance IPAM prefixes for srsRAN ==="
echo "  Networks: N2=172.2.0.0/24, N3=172.3.0.0/24,"
echo "            E1=172.4.0.0/24, F1C=172.5.0.0/24, F1U=172.6.0.0/24"

sleep 5   # give NetworkInstances time to be present

# vpc-ran: N2 (NGAP) and N3 (GTP-U) live here.
echo "  Patching vpc-ran with N2 and N3 prefixes ..."
kubectl patch networkinstances.ipam.resource.nephio.org vpc-ran \
  --type=json -p='[
    {"op": "add", "path": "/spec/prefixes/-", "value": {
      "prefix": "172.2.0.0/24",
      "labels": {
        "nephio.org/network-name": "n2",
        "nephio.org/address-family": "ipv4",
        "nephio.org/cluster-name": "'"${CLUSTER_NAME}"'"
      }
    }},
    {"op": "add", "path": "/spec/prefixes/-", "value": {
      "prefix": "172.3.0.0/24",
      "labels": {
        "nephio.org/network-name": "n3",
        "nephio.org/address-family": "ipv4",
        "nephio.org/cluster-name": "'"${CLUSTER_NAME}"'"
      }
    }}
  ]' 2>/dev/null || echo "  Warning: could not patch vpc-ran"

# vpc-internal: E1, F1C, F1U live here (all intra-cluster inter-pod).
echo "  Patching vpc-internal with E1, F1C, F1U prefixes ..."
kubectl patch networkinstances.ipam.resource.nephio.org vpc-internal \
  --type=json -p='[
    {"op": "add", "path": "/spec/prefixes/-", "value": {
      "prefix": "172.4.0.0/24",
      "labels": {
        "nephio.org/network-name": "e1",
        "nephio.org/address-family": "ipv4",
        "nephio.org/cluster-name": "'"${CLUSTER_NAME}"'"
      }
    }},
    {"op": "add", "path": "/spec/prefixes/-", "value": {
      "prefix": "172.5.0.0/24",
      "labels": {
        "nephio.org/network-name": "f1c",
        "nephio.org/address-family": "ipv4",
        "nephio.org/cluster-name": "'"${CLUSTER_NAME}"'"
      }
    }},
    {"op": "add", "path": "/spec/prefixes/-", "value": {
      "prefix": "172.6.0.0/24",
      "labels": {
        "nephio.org/network-name": "f1u",
        "nephio.org/address-family": "ipv4",
        "nephio.org/cluster-name": "'"${CLUSTER_NAME}"'"
      }
    }}
  ]' 2>/dev/null || echo "  Warning: could not patch vpc-internal"

echo "✓ NetworkInstance prefixes patched"
echo "  Waiting 15 s for IPAM to process ..."
sleep 15

# ── Step 4: PackageVariantSet ────────────────────────────────────────────────
echo ""
echo "=== Step 4: Create srsRAN PackageVariantSet ==="

cat > /tmp/srsran-pvset.yaml <<EOF
apiVersion: config.porch.kpt.dev/v1alpha2
kind: PackageVariantSet
metadata:
  name: srsran-${CLUSTER_NAME}
spec:
  upstream:
    repo: ${SRSRAN_REPO}
    package: ${SRSRAN_PKG}
    workspaceName: ${BLUEPRINT_WORKSPACE}
  targets:
  - objectSelector:
      apiVersion: infra.nephio.org/v1alpha1
      kind: WorkloadCluster
      matchLabels:
        nephio.org/site-type: ${SITE_TYPE}
    template:
      downstream:
        package: srsran
      annotations:
        approval.nephio.org/policy: always
      injectors:
      - nameExpr: target.name
EOF

kubectl apply -f /tmp/srsran-pvset.yaml
echo "✓ PackageVariantSet applied: srsran-${CLUSTER_NAME}"

# ── Step 5: Propose + approve package revision ────────────────────────────────
echo ""
echo "=== Step 5: Propose and approve the generated PackageRevision ==="
echo "  Waiting 30 s for Porch to generate the package revision ..."
sleep 30

SRSRAN_REV=""
for attempt in 1 2 3 4 5; do
  SRSRAN_REV=$(porchctl rpkg get -n default 2>/dev/null \
    | grep "${CLUSTER_NAME}\.srsran\." \
    | grep -v "Published" \
    | awk '{print $1}' \
    | head -1)
  if [ -n "${SRSRAN_REV}" ]; then
    break
  fi
  echo "  Attempt ${attempt}: revision not found yet, waiting 15 s ..."
  sleep 15
done

if [ -z "${SRSRAN_REV}" ]; then
  echo "WARNING: srsRAN PackageRevision not found after 5 attempts."
  echo "  Check: porchctl rpkg get -n default | grep srsran"
  echo "  Then manually: porchctl rpkg propose <rev> -n default"
  echo "                 porchctl rpkg approve <rev> -n default"
else
  echo "  Found revision: ${SRSRAN_REV}"
  porchctl rpkg propose "${SRSRAN_REV}" -n default
  echo "  Proposed."
  porchctl rpkg approve "${SRSRAN_REV}" -n default
  echo "✓ PackageRevision approved: ${SRSRAN_REV}"
fi

# ── Step 6: Wait for pods ─────────────────────────────────────────────────────
echo ""
echo "=== Step 6: Wait for srsRAN pods to become Ready ==="
echo "  Waiting up to 3 min for ConfigSync to apply the package ..."

SRSRAN_NS="srsran-${CLUSTER_NAME}"
for component in cucp cuup du; do
  DEP="srsran-${CLUSTER_NAME}-${component}"
  echo -n "  Waiting for deployment/${DEP} in ${SRSRAN_NS} ... "
  kubectl wait deployment "${DEP}" \
    --namespace="${SRSRAN_NS}" \
    --for=condition=Available \
    --timeout=180s 2>/dev/null && echo "Ready" || echo "Timeout (check manually)"
done

# ── Step 7: Status summary ───────────────────────────────────────────────────
echo ""
echo "=========================================="
echo "  Deployment Summary"
echo "=========================================="
echo ""
export KUBECONFIG="./${KUBECONFIG_FILE}"

echo "srsRAN pods (namespace: ${SRSRAN_NS}):"
kubectl get pods -n "${SRSRAN_NS}" 2>/dev/null \
  | grep -E "NAME|cucp|cuup|du|ue|radiobreaker" \
  || echo "  Namespace ${SRSRAN_NS} not found yet"

echo ""
echo "SrsRanDeployment CR:"
kubectl get srsrandeployment -n "${SRSRAN_NS}" 2>/dev/null || true

echo ""
echo "Services:"
kubectl get svc -n "${SRSRAN_NS}" 2>/dev/null || true

echo ""
echo "=========================================="
echo "  Troubleshooting"
echo "=========================================="
echo ""
echo "Check all srsRAN pods:"
echo "  export KUBECONFIG=./${KUBECONFIG_FILE}"
echo "  kubectl get pods -n ${SRSRAN_NS}"
echo ""
echo "Check srsran-operator logs:"
echo "  kubectl logs -n srsran deployment/srsran-operator --tail=50"
echo ""
echo "Inspect the SrsRanDeployment CR (verify IPAM-injected IPs):"
echo "  kubectl get srsrandeployment -n ${SRSRAN_NS} -o yaml | grep -A20 'interfaces:'"
echo ""
echo "Check PackageRevision status:"
echo "  porchctl rpkg get -n default | grep srsran"
echo ""
echo "Manually approve if needed:"
echo "  porchctl rpkg propose  <rev> -n default"
echo "  porchctl rpkg approve  <rev> -n default"
echo ""
echo "Re-run with a different cluster:"
echo "  CLUSTER_NAME=edge1 SITE_TYPE=edge ${0}"
echo ""
echo "=========================================="
echo "  Interface → IP prefix mapping"
echo "=========================================="
echo "  N2  (CU-CP  NGAP→AMF)        172.2.0.0/24  vpc-ran"
echo "  N3  (CU-UP  GTP-U→UPF)       172.3.0.0/24  vpc-ran"
echo "  E1  (CU-CP↔CU-UP E1AP)        172.4.0.0/24  vpc-internal"
echo "  F1C (CU-CP↔DU   F1-AP ctrl)   172.5.0.0/24  vpc-internal"
echo "  F1U (CU-UP↔DU   F1-U  data)   172.6.0.0/24  vpc-internal"
echo "=========================================="
