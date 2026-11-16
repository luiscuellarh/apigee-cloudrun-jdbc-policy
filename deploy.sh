#!/usr/bin/env bash
# ==============================================================================
# deploy.sh: Master End-to-End Orchestrator
#   Step 1: Zero-to-One GCP APIs, Service Accounts & IAM Bindings
#   Step 2: Private Always-Warm Cloud Run JDBC Connector (--set-custom-audiences)
#   Step 3: Dynamic Apigee Discovery + Encrypted KVM + PropertySet + AFT Proxies
# ==============================================================================
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

"${SCRIPT_DIR}/scripts/01-setup-gcp.sh" "$@"
"${SCRIPT_DIR}/scripts/02-deploy-cloudrun.sh" "$@"
"${SCRIPT_DIR}/scripts/03-deploy-apigee.sh" "$@"

echo ""
echo "🎉 Full deployment complete!"
echo "   -> Run './test.sh' to verify the built-in & live public PostgreSQL flows."
echo "   -> (Optional) Run './scripts/04-optional-test-db-vm.sh --create' to spin up"
echo "      and test a Private Compute Engine PostgreSQL VM over Direct VPC Egress."
