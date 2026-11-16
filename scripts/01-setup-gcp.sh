#!/usr/bin/env bash
# ==============================================================================
# 01-setup-gcp.sh: Zero-to-One GCP API Enablement & IAM Service Account Setup
# ==============================================================================
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/00-env.sh
source "${SCRIPT_DIR}/00-env.sh" "$@"

echo "=============================================================================="
echo " [Step 1/3] Zero-to-One GCP APIs & Service Account Provisioning"
echo " Project ID            : ${PROJECT_ID}"
echo " Region                : ${REGION}"
echo " Cloud Run Runtime SA  : ${RUNTIME_SA_EMAIL}"
echo " Apigee Invoker SA     : ${INVOKER_SA_EMAIL}"
echo "=============================================================================="

gcloud config set project "${PROJECT_ID}" --quiet

echo "🛠️  [1.1] Enabling required Google Cloud APIs..."
gcloud services enable \
  run.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  iam.googleapis.com \
  iamcredentials.googleapis.com \
  cloudresourcemanager.googleapis.com \
  apigee.googleapis.com \
  --project="${PROJECT_ID}" \
  --quiet

ACTIVE_USER="$(gcloud config get-value account 2>/dev/null || true)"
PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" --format="value(projectNumber)")"
COMPUTE_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
APIGEE_SERVICE_AGENT="service-${PROJECT_NUMBER}@gcp-sa-apigee.iam.gserviceaccount.com"

echo "🔐 [1.2] Configuring Project & Cloud Build IAM permissions..."
if [[ -n "${ACTIVE_USER}" ]]; then
  for ROLE in \
    roles/run.admin \
    roles/cloudbuild.builds.editor \
    roles/apigee.admin \
    roles/iam.serviceAccountAdmin \
    roles/iam.serviceAccountUser \
    roles/storage.admin; do
    gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
      --member="user:${ACTIVE_USER}" \
      --role="${ROLE}" \
      --condition=None \
      --quiet >/dev/null 2>&1 || true
  done
fi

for ROLE in \
  roles/storage.admin \
  roles/artifactregistry.writer \
  roles/logging.logWriter \
  roles/cloudbuild.builds.builder \
  roles/run.admin \
  roles/iam.serviceAccountUser \
  roles/apigee.admin; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="serviceAccount:${COMPUTE_SA}" \
    --role="${ROLE}" \
    --condition=None \
    --quiet >/dev/null 2>&1 || true
done

echo "👤 [1.3] Creating Cloud Run Runtime Service Account (${RUNTIME_SA_EMAIL})..."
if ! gcloud iam service-accounts describe "${RUNTIME_SA_EMAIL}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud iam service-accounts create "${RUNTIME_SA_NAME}" \
    --display-name="Cloud Run JDBC Policy Connector Runtime SA" \
    --project="${PROJECT_ID}" \
    --quiet
  sleep 5
fi

# Grant logging, metrics, and Apigee KVM read access so Cloud Run auto-hydrates pools on startup
for ROLE in roles/logging.logWriter roles/monitoring.metricWriter roles/apigee.environmentAdmin; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="serviceAccount:${RUNTIME_SA_EMAIL}" \
    --role="${ROLE}" \
    --condition=None \
    --quiet >/dev/null 2>&1 || true
done

echo "🔑 [1.4] Creating Apigee Proxy Invoker Service Account (${INVOKER_SA_EMAIL})..."
if ! gcloud iam service-accounts describe "${INVOKER_SA_EMAIL}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud iam service-accounts create "${INVOKER_SA_NAME}" \
    --display-name="Apigee X to Cloud Run JDBC Invoker SA" \
    --project="${PROJECT_ID}" \
    --quiet
  sleep 5
fi

if [[ -n "${ACTIVE_USER}" ]]; then
  for SA in "${RUNTIME_SA_EMAIL}" "${INVOKER_SA_EMAIL}"; do
    gcloud iam service-accounts add-iam-policy-binding "${SA}" \
      --project="${PROJECT_ID}" \
      --member="user:${ACTIVE_USER}" \
      --role="roles/iam.serviceAccountUser" \
      --condition=None \
      --quiet >/dev/null 2>&1 || true
  done
fi

for ROLE in roles/iam.serviceAccountTokenCreator roles/iam.serviceAccountUser; do
  gcloud iam service-accounts add-iam-policy-binding "${INVOKER_SA_EMAIL}" \
    --project="${PROJECT_ID}" \
    --member="serviceAccount:${APIGEE_SERVICE_AGENT}" \
    --role="${ROLE}" \
    --condition=None \
    --quiet >/dev/null 2>&1 || true
done

echo "✅ [Step 1/3] GCP APIs and Service Accounts provisioned successfully."
