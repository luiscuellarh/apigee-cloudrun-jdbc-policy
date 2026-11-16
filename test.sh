#!/usr/bin/env bash
# ==============================================================================
# test.sh: Automated End-to-End Verification Suite with Response Validation
#   Part 1: Step 1 — Configure Datasource & Warm Pool via Admin Proxy (jdbc-admin-v1)
#   Part 2: Step 2 — Consume Warm Pool from API Proxy via PropertySet (jdbc.properties)
#                    including Single-Row SELECT, DML Transactions, and Multi-Row
#                    Large SELECT Result Sets (rows[] + truncated guard)
# ==============================================================================
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/00-env.sh
source "${SCRIPT_DIR}/scripts/00-env.sh" "$@"

if [[ -z "${APIGEE_HOSTNAME:-}" ]]; then
  echo "❌ ERROR: Could not resolve APIGEE_HOSTNAME. Pass --hostname YOUR_APIGEE_DOMAIN." >&2
  exit 1
fi

ADMIN_URL="https://${APIGEE_HOSTNAME}${ADMIN_BASE_PATH}/datasources"
POLICY_URL="https://${APIGEE_HOSTNAME}${POLICY_BASE_PATH}"

PASSED=0
FAILED=0

run_test() {
  local title="$1"
  local expected_code="$2"
  local jq_expr="$3"
  shift 3

  echo "------------------------------------------------------------------------------"
  echo "${title}"
  local resp_file="/tmp/jdbc_test_resp.json"
  local http_code
  http_code="$(curl -sk -o "${resp_file}" -w "%{http_code}" "$@")"

  jq . "${resp_file}" 2>/dev/null || cat "${resp_file}"
  echo "HTTP Status: ${http_code} (Expected: ${expected_code})"

  if [[ "${http_code}" == "${expected_code}" ]] && jq -e "${jq_expr}" "${resp_file}" >/dev/null 2>&1; then
    echo "✅ PASS"
    PASSED=$((PASSED + 1))
  else
    echo "❌ FAIL (Expected HTTP ${expected_code} and jq '${jq_expr}')"
    FAILED=$((FAILED + 1))
  fi
  echo ""
}

echo "=============================================================================="
echo " End-to-End JDBC/ODBC Policy Verification Suite"
echo " Discovered Apigee Env      : ${APIGEE_ENV}"
echo " Discovered Apigee Hostname : ${APIGEE_HOSTNAME}"
echo " Admin Proxy Endpoint       : ${ADMIN_URL}"
echo " Runtime Policy Endpoint    : ${POLICY_URL}"
echo "=============================================================================="

run_test "🔧 [Admin Test 1] List Initial Warm Datasource Pools (enterprise-sqlite)" \
  "200" '.datasources | length >= 1' \
  "${ADMIN_URL}"

run_test "🔧 [Admin Test 2] Configure Live Public PostgreSQL ('pg-rna-public' -> hh-pgsql-public.ebi.ac.uk:5432) & Pre-Open 3 Warm Connections" \
  "200" '.status == "registered_and_warmed" and .open_connections == 3' \
  -X POST "${ADMIN_URL}" \
  -H "Content-Type: application/json" \
  -d '{
    "datasource_id": "pg-rna-public",
    "description": "Public EBI RNAcentral PostgreSQL Service - Persistent Warm Pool",
    "driver": "postgres",
    "jdbc_url": "jdbc:postgresql://hh-pgsql-public.ebi.ac.uk:5432/pfmegrnargs",
    "username": "reader",
    "password": "NWDMCE5xdipIjRrp",
    "min_pool_size": 3,
    "max_pool_size": 10,
    "keep_alive_sec": 30,
    "query_timeout_ms": 10000
  }'

run_test "🔧 [Admin Test 3] Verify 'pg-rna-public' Pool Health & Masked Credentials" \
  "200" '.datasource_id == "pg-rna-public" and .healthy == true and (.masked_dsn | contains("***") or contains("%2A%2A%2A"))' \
  "${ADMIN_URL}?id=pg-rna-public"

run_test "🔧 [Admin Test 4] Verify Admin Proxy Conditional Guard Rejects Missing datasource_id (-> 400 Bad Request)" \
  "400" '.error_code == "MISSING_DATASOURCE_ID"' \
  -X POST "${ADMIN_URL}" \
  -H "Content-Type: application/json" \
  -d '{"driver": "postgres", "jdbc_url": "jdbc:postgresql://localhost:5432/db"}'

echo "=============================================================================="
echo " PART 2: CONSUME WARM POOLS FROM API PROXY (USING jdbc.properties PROPERTYSET)"
echo "=============================================================================="

run_test "🧪 [Runtime Test 5] SELECT Subscriber (?id=1001) via {propertyset.jdbc.datasource_id} (-> 200 OK)" \
  "200" '.extracted_context_variables["jdbc.full_name"] == "Elena Rostova" and (.rows | length == 1)' \
  "${POLICY_URL}/subscribers?id=1001"

run_test "🧪 [Runtime Test 6] SELECT Non-Existent Subscriber (?id=9999) with fail_if_empty=true (-> 404 Not Found)" \
  "404" '.error_code == "EMPTY_RESULT_SET"' \
  "${POLICY_URL}/subscribers?id=9999"

run_test "🧪 [Runtime Test 7] Parameterized INSERT Order DML with Transaction (POST /orders -> 200 OK)" \
  "200" '.operation_type == "DML" and .extracted_context_variables["jdbc.row_count"] == "1"' \
  -X POST "${POLICY_URL}/orders" \
  -H "Content-Type: application/json" \
  -d '{
    "subscriber_id": 1001,
    "sku": "DEDICATED-CLOUD-INTERCONNECT-10G",
    "amount": 3150.00,
    "currency": "EUR"
  }'

run_test "🌐 [Runtime Test 8] Single-Row SELECT on Live External PostgreSQL (?upi=URS0000000001) (-> 200 OK)" \
  "200" '.datasource_id == "pg-rna-public" and .extracted_context_variables["jdbc.upi"] == "URS0000000001"' \
  "${POLICY_URL}/rna?upi=URS0000000001"

run_test "📦 [Runtime Test 9] Multi-Row / Large SELECT on Live External PostgreSQL (?rna_type=rRNA) with max_rows Cap, rows[] Array & truncated=true (-> 200 OK)" \
  "200" '.extracted_context_variables["jdbc.row_count"] == "3" and .extracted_context_variables["jdbc.truncated"] == "true" and (.rows | length == 3)' \
  "${POLICY_URL}/rna-search?rna_type=rRNA"

echo "=============================================================================="
echo " Verification Summary: ${PASSED} Passed, ${FAILED} Failed"
echo "=============================================================================="
if [[ "${FAILED}" -gt 0 ]]; then
  exit 1
fi
