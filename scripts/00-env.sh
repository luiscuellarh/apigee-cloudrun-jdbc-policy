#!/usr/bin/env bash
# ==============================================================================
# 00-env.sh: Shared Configuration & Dynamic Apigee Management API Discovery
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)   export PROJECT_ID="$2"; shift 2 ;;
    --region)    export REGION="$2"; shift 2 ;;
    --env)       export APIGEE_ENV="$2"; shift 2 ;;
    --hostname)  export APIGEE_HOSTNAME="$2"; shift 2 ;;
    --audience)  export CUSTOM_AUDIENCE="$2"; shift 2 ;;
    --service)   export SERVICE_NAME="$2"; shift 2 ;;
    --network)   export VPC_NETWORK="$2"; shift 2 ;;
    --subnet)    export VPC_SUBNET="$2"; shift 2 ;;
    *) shift ;;
  esac
done

export PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null || true)}"
if [[ -z "${PROJECT_ID}" || "${PROJECT_ID}" == "(unset)" ]]; then
  echo "❌ ERROR: PROJECT_ID is not set. Run 'gcloud config set project YOUR_PROJECT_ID' or pass '--project YOUR_PROJECT_ID'." >&2
  exit 1
fi

export APIGEE_ORG="${APIGEE_ORG:-${PROJECT_ID}}"
export REGION="${REGION:-europe-west2}"
export SERVICE_NAME="${SERVICE_NAME:-jdbc-policy-connector}"
export CUSTOM_AUDIENCE="${CUSTOM_AUDIENCE:-jdbc-policy-service}"

# Optional Direct VPC Egress settings for On-Premises / Private VPC Databases
export VPC_NETWORK="${VPC_NETWORK:-}"
export VPC_SUBNET="${VPC_SUBNET:-}"

# Dedicated Least-Privilege Service Accounts
export RUNTIME_SA_NAME="${RUNTIME_SA_NAME:-jdbc-cloudrun-runtime}"
export RUNTIME_SA_EMAIL="${RUNTIME_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

export INVOKER_SA_NAME="${INVOKER_SA_NAME:-apigee-jdbc-invoker}"
export INVOKER_SA_EMAIL="${INVOKER_SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

# Apigee Proxies, Encrypted KVM & PropertySet
export KVM_NAME="${KVM_NAME:-jdbc-datasources-kvm}"
export PROPERTYSET_NAME="${PROPERTYSET_NAME:-jdbc}"
export ADMIN_PROXY_NAME="${ADMIN_PROXY_NAME:-jdbc-admin-v1}"
export ADMIN_BASE_PATH="${ADMIN_BASE_PATH:-/v1/jdbc-admin}"
export POLICY_PROXY_NAME="${POLICY_PROXY_NAME:-jdbc-policy-callout-v1}"
export POLICY_BASE_PATH="${POLICY_BASE_PATH:-/v1/jdbc-policy}"

# Optional Test DB VM Settings (04-optional-test-db-vm.sh)
export TEST_DB_VM_NAME="${TEST_DB_VM_NAME:-jdbc-onprem-sim-db}"
export TEST_DB_ZONE="${TEST_DB_ZONE:-${REGION}-b}"
export TEST_DB_FIREWALL_RULE="${TEST_DB_FIREWALL_RULE:-allow-cloudrun-to-jdbc-vm}"

# Dynamic Apigee Environment & Hostname Auto-Discovery
discover_apigee_topology() {
  local token="${APIGEE_TOKEN:-$(gcloud auth print-access-token 2>/dev/null || true)}"
  if [[ -z "${token}" ]]; then
    return 0
  fi

  if [[ -z "${APIGEE_ENV:-}" ]]; then
    local envs_json
    envs_json="$(curl -sf -H "Authorization: Bearer ${token}" \
      "https://apigee.googleapis.com/v1/organizations/${APIGEE_ORG}/environments" 2>/dev/null || echo "[]")"
    export APIGEE_ENV="$(echo "${envs_json}" | jq -r '.[0] // empty')"
  fi

  if [[ -z "${APIGEE_HOSTNAME:-}" && -n "${APIGEE_ENV:-}" ]]; then
    local eg_json
    eg_json="$(curl -sf -H "Authorization: Bearer ${token}" \
      "https://apigee.googleapis.com/v1/organizations/${APIGEE_ORG}/envgroups" 2>/dev/null || echo "{}")"

    for eg in $(echo "${eg_json}" | jq -r '.environmentGroups[]?.name // empty'); do
      local att_json
      att_json="$(curl -sf -H "Authorization: Bearer ${token}" \
        "https://apigee.googleapis.com/v1/organizations/${APIGEE_ORG}/envgroups/${eg}/attachments" 2>/dev/null || echo "{}")"
      local matched_env
      matched_env="$(echo "${att_json}" | jq -r --arg env "${APIGEE_ENV}" \
        '.environmentGroupAttachments[]? | select(.environment == $env) | .environment' | head -n 1)"

      if [[ "${matched_env}" == "${APIGEE_ENV}" ]]; then
        export APIGEE_HOSTNAME="$(echo "${eg_json}" | jq -r --arg eg "${eg}" \
          '.environmentGroups[]? | select(.name == $eg) | .hostnames[0] // empty')"
        export APIGEE_HOST_DISPLAY="${APIGEE_HOSTNAME} (envgroup: ${eg})"
        break
      fi
    done
  fi
}

discover_apigee_topology
