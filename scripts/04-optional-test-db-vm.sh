#!/usr/bin/env bash
# ==============================================================================
# 04-optional-test-db-vm.sh: Optional Private Compute Engine PostgreSQL VM
#   Simulates an On-Premises / Private VPC PostgreSQL Database (10.x.x.x:5432)
#   accessed from Cloud Run via Direct VPC Egress (--vpc-egress=private-ranges-only).
#
# Usage:
#   ./scripts/04-optional-test-db-vm.sh --create   # Provision VM, enable Direct VPC Egress & test via Apigee
#   ./scripts/04-optional-test-db-vm.sh --delete   # Delete VM, firewall rule & datasource registration
# ==============================================================================
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ACTION="create"
for arg in "$@"; do
  case "${arg}" in
    --create|--deploy) ACTION="create" ;;
    --delete|--cleanup|--destroy) ACTION="delete" ;;
  esac
done

# shellcheck source=scripts/00-env.sh
source "${SCRIPT_DIR}/00-env.sh" "$@"

ADMIN_URL="https://${APIGEE_HOSTNAME}${ADMIN_BASE_PATH}/datasources"
POLICY_URL="https://${APIGEE_HOSTNAME}${POLICY_BASE_PATH}"

# Auto-discover VPC Network if not explicitly provided
if [[ -z "${VPC_NETWORK:-}" ]]; then
  if gcloud compute networks describe default --project="${PROJECT_ID}" >/dev/null 2>&1; then
    VPC_NET="default"
  else
    VPC_NET="$(gcloud compute networks list --project="${PROJECT_ID}" --format="value(name)" 2>/dev/null | head -n 1 || true)"
    VPC_NET="${VPC_NET:-jdbc-test-vpc}"
  fi
else
  VPC_NET="${VPC_NETWORK}"
fi

# Auto-discover Regional Subnet in ${REGION} (or use jdbc-test-subnet-${REGION})
if [[ -z "${VPC_SUBNET:-}" ]]; then
  EXISTING_SUB="$(gcloud compute networks subnets list \
    --project="${PROJECT_ID}" \
    --network="${VPC_NET}" \
    --regions="${REGION}" \
    --format="value(name)" 2>/dev/null | head -n 1 || true)"
  VPC_SUB="${EXISTING_SUB:-jdbc-test-subnet-${REGION}}"
else
  VPC_SUB="${VPC_SUBNET}"
fi

if [[ "${ACTION}" == "delete" ]]; then
  echo "=============================================================================="
  echo " [Optional Cleanup] Deleting Private PostgreSQL Test VM & Datasource"
  echo " Project ID   : ${PROJECT_ID}"
  echo " VM Name      : ${TEST_DB_VM_NAME} (${TEST_DB_ZONE})"
  echo " Firewall Rule: ${TEST_DB_FIREWALL_RULE}"
  echo "=============================================================================="

  echo "🗑️  [1/3] Removing 'private-vm-postgres' datasource via Apigee Admin Proxy..."
  curl -sk -X DELETE "${ADMIN_URL}?id=private-vm-postgres" >/dev/null 2>&1 || true

  echo "🗑️  [2/3] Deleting Compute Engine VM '${TEST_DB_VM_NAME}'..."
  if gcloud compute instances describe "${TEST_DB_VM_NAME}" --zone="${TEST_DB_ZONE}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    gcloud compute instances delete "${TEST_DB_VM_NAME}" \
      --zone="${TEST_DB_ZONE}" \
      --project="${PROJECT_ID}" \
      --quiet
  else
    echo "   -> VM '${TEST_DB_VM_NAME}' already deleted."
  fi

  echo "🗑️  [3/3] Deleting firewall rule '${TEST_DB_FIREWALL_RULE}'..."
  if gcloud compute firewall-rules describe "${TEST_DB_FIREWALL_RULE}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    gcloud compute firewall-rules delete "${TEST_DB_FIREWALL_RULE}" \
      --project="${PROJECT_ID}" \
      --quiet
  else
    echo "   -> Firewall rule '${TEST_DB_FIREWALL_RULE}' already deleted."
  fi

  echo "✅ [Optional Cleanup] Private PostgreSQL VM and firewall rule deleted."
  exit 0
fi

echo "=============================================================================="
echo " [Optional Step 4] Provisioning Private PostgreSQL Container VM (On-Prem Simulation)"
echo " Project ID            : ${PROJECT_ID}"
echo " VM Name & Zone        : ${TEST_DB_VM_NAME} (${TEST_DB_ZONE})"
echo " VPC Network / Subnet  : ${VPC_NET} / ${VPC_SUB}"
echo " Datasource ID         : private-vm-postgres (bound in jdbc.properties)"
echo "=============================================================================="

echo "🛠️  [4.1] Enabling Compute Engine API & granting required IAM roles..."
gcloud services enable compute.googleapis.com --project="${PROJECT_ID}" --quiet

ACTIVE_USER="$(gcloud config get-value account 2>/dev/null || true)"
PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)")"
CLOUD_RUN_SERVICE_AGENT="service-${PROJECT_NUMBER}@serverless-robot-prod.iam.gserviceaccount.com"

if [[ -n "${ACTIVE_USER}" ]]; then
  for ROLE in roles/compute.instanceAdmin.v1 roles/compute.networkAdmin roles/compute.securityAdmin; do
    gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
      --member="user:${ACTIVE_USER}" \
      --role="${ROLE}" \
      --condition=None \
      --quiet >/dev/null 2>&1 || true
  done
fi

# Grant Cloud Run Service Agent permission to attach Direct VPC Egress interfaces
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
  --member="serviceAccount:${CLOUD_RUN_SERVICE_AGENT}" \
  --role="roles/compute.networkUser" \
  --condition=None \
  --quiet >/dev/null 2>&1 || true

echo "🌐 [4.2] Ensuring VPC network '${VPC_NET}' and regional subnet '${VPC_SUB}' (${REGION}) exist with Private Google Access..."
if ! gcloud compute networks describe "${VPC_NET}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud compute networks create "${VPC_NET}" \
    --project="${PROJECT_ID}" \
    --subnet-mode=custom \
    --quiet
fi

if ! gcloud compute networks subnets describe "${VPC_SUB}" --region="${REGION}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud compute networks subnets create "${VPC_SUB}" \
    --project="${PROJECT_ID}" \
    --network="${VPC_NET}" \
    --region="${REGION}" \
    --range="10.240.0.0/24" \
    --enable-private-ip-google-access \
    --quiet
else
  gcloud compute networks subnets update "${VPC_SUB}" \
    --region="${REGION}" \
    --project="${PROJECT_ID}" \
    --enable-private-ip-google-access \
    --quiet
fi

SUBNET_CIDR="$(gcloud compute networks subnets describe "${VPC_SUB}" \
  --region="${REGION}" \
  --project="${PROJECT_ID}" \
  --format="value(ipCidrRange)")"

if ! gcloud compute firewall-rules describe "${TEST_DB_FIREWALL_RULE}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud compute firewall-rules create "${TEST_DB_FIREWALL_RULE}" \
    --project="${PROJECT_ID}" \
    --network="${VPC_NET}" \
    --direction=INGRESS \
    --action=ALLOW \
    --rules=tcp:5432 \
    --source-ranges="${SUBNET_CIDR},10.0.0.0/8" \
    --target-tags="jdbc-test-db" \
    --quiet
fi

echo "🐳 [4.3] Creating Private Container-Optimized OS VM '${TEST_DB_VM_NAME}' (--no-address) running PostgreSQL 16..."
cat <<'EOS' > /tmp/pg-vm-startup.sh
#!/bin/bash
cat <<'SQL' > /tmp/init.sql
CREATE TABLE IF NOT EXISTS billing_accounts (
  account_id VARCHAR(32) PRIMARY KEY,
  company_name VARCHAR(128) NOT NULL,
  region VARCHAR(64) NOT NULL,
  balance_eur NUMERIC(12,2) NOT NULL,
  tier VARCHAR(32) NOT NULL
);
INSERT INTO billing_accounts (account_id, company_name, region, balance_eur, tier) VALUES
  ('ACC-9001', 'Global Telco Holdings S.A.', 'EMEA-WEST', 148500.75, 'DIAMOND_CARRIER'),
  ('ACC-9002', 'Andean Cloud Logistics', 'LATAM-NORTH', 54200.00, 'PLATINUM_WAN')
ON CONFLICT (account_id) DO NOTHING;
SQL
chmod 644 /tmp/init.sql

docker rm -f pg-onprem-sim >/dev/null 2>&1 || true
docker run -d --name pg-onprem-sim \
  --restart=always \
  -p 5432:5432 \
  -v /tmp/init.sql:/docker-entrypoint-initdb.d/init.sql:ro \
  -e POSTGRES_DB=enterprise_db \
  -e POSTGRES_USER=apigee_jdbc \
  -e POSTGRES_PASSWORD=OnPremPass2026 \
  mirror.gcr.io/library/postgres:16-alpine
EOS

if ! gcloud compute instances describe "${TEST_DB_VM_NAME}" --zone="${TEST_DB_ZONE}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud compute instances create "${TEST_DB_VM_NAME}" \
    --project="${PROJECT_ID}" \
    --zone="${TEST_DB_ZONE}" \
    --machine-type=e2-micro \
    --network="${VPC_NET}" \
    --subnet="${VPC_SUB}" \
    --no-address \
    --shielded-secure-boot \
    --shielded-vtpm \
    --shielded-integrity-monitoring \
    --service-account="${RUNTIME_SA_EMAIL}" \
    --scopes="https://www.googleapis.com/auth/cloud-platform" \
    --image-family=cos-stable \
    --image-project=cos-cloud \
    --tags="jdbc-test-db" \
    --metadata-from-file="startup-script=/tmp/pg-vm-startup.sh" \
    --quiet
fi

VM_INTERNAL_IP="$(gcloud compute instances describe "${TEST_DB_VM_NAME}" \
  --zone="${TEST_DB_ZONE}" \
  --project="${PROJECT_ID}" \
  --format="value(networkInterfaces[0].networkIP)")"

echo "   -> VM Internal IP : ${VM_INTERNAL_IP}"

echo "🔌 [4.4] Attaching Cloud Run Direct VPC Egress (--network=${VPC_NET} --subnet=${VPC_SUB}) to '${SERVICE_NAME}'..."
gcloud run services update "${SERVICE_NAME}" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --network="${VPC_NET}" \
  --subnet="${VPC_SUB}" \
  --vpc-egress=private-ranges-only \
  --quiet

echo "⏳ [4.5] Waiting for PostgreSQL container on ${VM_INTERNAL_IP}:5432 to finish initializing..."
for attempt in $(seq 1 18); do
  HTTP_CODE="$(curl -sk -o /tmp/vm_reg_out.json -w "%{http_code}" -X POST "${ADMIN_URL}" \
    -H "Content-Type: application/json" \
    -d "{
      \"datasource_id\": \"private-vm-postgres\",
      \"description\": \"Private Compute Engine PostgreSQL 16 Container VM over Direct VPC Egress\",
      \"driver\": \"postgres\",
      \"jdbc_url\": \"jdbc:postgresql://${VM_INTERNAL_IP}:5432/enterprise_db\",
      \"username\": \"apigee_jdbc\",
      \"password\": \"OnPremPass2026\",
      \"min_pool_size\": 3,
      \"max_pool_size\": 10,
      \"keep_alive_sec\": 30,
      \"properties\": {\"sslmode\": \"disable\"}
    }")"
  if [[ "${HTTP_CODE}" == "200" ]]; then
    echo "   -> Registered 'private-vm-postgres' & warmed persistent pool:"
    jq . /tmp/vm_reg_out.json 2>/dev/null || cat /tmp/vm_reg_out.json
    break
  fi
  sleep 5
done

echo ""
echo "🧪 [4.6] Testing Runtime Apigee Proxy Callout to Private PostgreSQL VM (GET ${POLICY_URL}/private-vm-accounts?account_id=ACC-9001)..."
curl -sk -w "\nHTTP Status: %{http_code}\n" "${POLICY_URL}/private-vm-accounts?account_id=ACC-9001"
echo ""
echo "✅ [Optional Step 4] Private PostgreSQL VM verified end-to-end over Direct VPC Egress!"
echo "💡 To delete this VM when finished, run: ./scripts/04-optional-test-db-vm.sh --delete"
