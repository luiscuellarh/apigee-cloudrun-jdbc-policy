#!/usr/bin/env bash
# ==============================================================================
# 02-deploy-cloudrun.sh: Build & Deploy Private Always-Warm Cloud Run JDBC Connector
#   - Deploys with --no-allow-unauthenticated, --set-custom-audiences,
#     --min-instances=1, and --no-cpu-throttling so persistent DB pools stay warm
#   - Supports optional Direct VPC Egress (--network / --subnet) for On-Prem DBs
#   - Binds roles/run.invoker strictly to Apigee Invoker SA
# ==============================================================================
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/00-env.sh
source "${SCRIPT_DIR}/00-env.sh" "$@"

echo "=============================================================================="
echo " [Step 2/3] Deploying Private Always-Warm Cloud Run JDBC Connector"
echo " Service Name          : ${SERVICE_NAME}"
echo " Source Directory      : ${REPO_ROOT}/src"
echo " Custom OIDC Audience  : ${CUSTOM_AUDIENCE}"
echo " Runtime SA            : ${RUNTIME_SA_EMAIL}"
echo " Authorized Invoker SA : ${INVOKER_SA_EMAIL}"
echo " Direct VPC Egress     : ${VPC_NETWORK:-disabled (enable via --network/--subnet)}"
echo "=============================================================================="

VPC_FLAGS=()
if [[ -n "${VPC_NETWORK}" ]]; then
  VPC_SUBNET_EFFECTIVE="${VPC_SUBNET:-${VPC_NETWORK}}"
  VPC_FLAGS=(
    "--network=${VPC_NETWORK}"
    "--subnet=${VPC_SUBNET_EFFECTIVE}"
    "--vpc-egress=private-ranges-only"
  )
fi

echo "🚀 [2.1] Building and deploying '${SERVICE_NAME}' to Cloud Run (${REGION})..."
gcloud run deploy "${SERVICE_NAME}" \
  --source="${REPO_ROOT}/src" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --service-account="${RUNTIME_SA_EMAIL}" \
  --no-allow-unauthenticated \
  --ingress="all" \
  --min-instances=1 \
  --max-instances=5 \
  --no-cpu-throttling \
  --memory=512Mi \
  --cpu=1 \
  --set-custom-audiences="${CUSTOM_AUDIENCE}" \
  --set-env-vars="JDBC_DATASOURCES_CONFIG_PATH=/etc/jdbc/datasources.json,APIGEE_ORG=${APIGEE_ORG},APIGEE_ENV=${APIGEE_ENV:-dev},APIGEE_KVM_NAME=${KVM_NAME}" \
  "${VPC_FLAGS[@]}" \
  --quiet

export CLOUD_RUN_URL="$(gcloud run services describe "${SERVICE_NAME}" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --format="value(status.url)")"

echo "🛡️  [2.2] Binding roles/run.invoker on '${SERVICE_NAME}' to ${INVOKER_SA_EMAIL}..."
gcloud run services add-iam-policy-binding "${SERVICE_NAME}" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --member="serviceAccount:${INVOKER_SA_EMAIL}" \
  --role="roles/run.invoker" \
  --quiet

echo "✅ [Step 2/3] Private Cloud Run JDBC Connector deployed:"
echo "   -> Service URL      : ${CLOUD_RUN_URL}"
echo "   -> Custom Audience  : ${CUSTOM_AUDIENCE}"
echo "   -> Invoker Identity : ${INVOKER_SA_EMAIL}"
