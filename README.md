# Apigee X JDBC/ODBC Policy Connector

[![Open In Colab](https://colab.research.google.com/assets/colab-badge.svg)](https://colab.research.google.com/github/luiscuellarh/apigee-cloudrun-jdbc-policy/blob/main/colab/deploy_cloudrun_apigee.ipynb)

An out-of-the-box **JDBC/ODBC Policy & Persistent Connection Pool** for **Apigee X**. Execute parameterized `SELECT` queries, transactional `INSERT`/`UPDATE`/`DELETE` statements, and stored procedures (`CALL`/`EXEC`) directly from any Apigee API proxy against **PostgreSQL**, **MySQL / MariaDB**, **Microsoft SQL Server**, **Oracle**, and **SQLite**—across both Cloud and On-Premises networks.

---

## Key Capabilities

| Capability | Description |
| :--- | :--- |
| **Multi-Engine SQL Support** | Built-in pure-Go drivers for **PostgreSQL** (`pgx/v5`), **MySQL / MariaDB**, **SQL Server**, **Oracle** (`go-ora/v2`), and **SQLite**—zero external C libraries or JDBC JAR management required. |
| **Persistent Warm Connection Pools** | Pre-opens `min_pool_size` sockets per datasource in parallel and maintains an active **multi-socket Keep-Alive heartbeat** (`30s` default) so firewalls, VPN tunnels, and DB idle timers never drop connections. |
| **Encrypted Apigee KVM Storage** | Database credentials and pool schemas are stored encrypted at rest in Apigee's **Environment KVM (`jdbc-datasources-kvm`)** and automatically hydrated by Cloud Run on startup or scale-out. |
| **PropertySet Database Routing** | API proxies select their target database via Apigee's native **`jdbc.properties` PropertySet** (`{propertyset.jdbc.datasource_id}`), keeping database identifiers decoupled from client requests and policy XML. |
| **Server-Side Prepared Statements** | Automatically compiles `$[param]` and `${param}` placeholders into native driver bind parameters (`$1`, `?`, `@p1`, `:1`) and caches `*sql.Stmt` handles per pool for single-round-trip execution (`0–3 ms` query time). |
| **Zero-Trust IAM & Custom Audience** | Secured via `--no-allow-unauthenticated`, dedicated least-privilege Service Accounts, and `<GoogleIDToken><Audience>jdbc-policy-service</Audience></GoogleIDToken>`. |

---

## Quickstart Deployment

Choose either **Option A (Interactive Google Colab UI)** or **Option B (One-Command CLI)**.

### Option A: One-Click Google Colab UI (No Local Setup Required)

[![Open In Colab](https://colab.research.google.com/assets/colab-badge.svg)](https://colab.research.google.com/github/luiscuellarh/apigee-cloudrun-jdbc-policy/blob/main/colab/deploy_cloudrun_apigee.ipynb)

Open [`colab/deploy_cloudrun_apigee.ipynb`](colab/deploy_cloudrun_apigee.ipynb) in **Google Colab** or **Colab Enterprise** to deploy and test the entire stack through interactive form cards (**▶️ Play** buttons):
1. **Step 1** — Project Configuration & Google Cloud Authentication
2. **Step 2** — Enable GCP APIs & Provision Least-Privilege Service Accounts
3. **Step 3** — Deploy Private Always-Warm Cloud Run JDBC Connector (`jdbc-policy-connector`)
4. **Step 4** — Auto-Discover Apigee X Environment & Ingress Hostname
5. **Step 5** — Deploy Encrypted KVM, `jdbc.properties` PropertySet & Reference Proxies
6. **Step 6** — Interactive End-to-End Verification Dashboard (`9` Automated Checks)
7. **Step 7 *(Optional)*** — Provision, Query & Tear Down a Private VPC PostgreSQL Container VM

---

### Option B: One-Command CLI Deployment

```bash
git clone https://github.com/luiscuellarh/apigee-cloudrun-jdbc-policy.git
cd apigee-cloudrun-jdbc-policy
chmod +x deploy.sh test.sh scripts/*.sh

# Auto-discovers your active gcloud project, Apigee environment, and ingress hostname
./deploy.sh

# Run the 9-test end-to-end verification suite
./test.sh
```

Or pass explicit target flags:
```bash
./deploy.sh \
  --project my-gcp-project \
  --region europe-west2 \
  --env dev \
  --hostname api.example.com \
  --audience jdbc-policy-service
```

---

## How It Works: Two-Step Operational Model

![JDBC Architecture Overview](docs/images/jdbc-architecture-overview.svg)

### Step 1 — Configure a Datasource & Warm Pool (`jdbc-admin-v1`)

Register a database connection once via the Admin Proxy (`/v1/jdbc-admin/datasources`). Apigee encrypts the configuration in `jdbc-datasources-kvm`, and Cloud Run immediately pre-opens `min_pool_size` persistent sockets and starts the background Keep-Alive heartbeat.

![Step 1 Sequence](docs/images/jdbc-admin-flow-sequence.svg)

```bash
curl -X POST "https://${APIGEE_HOSTNAME}/v1/jdbc-admin/datasources" \
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
```

**Response (`200 OK`):**
```json
{
  "datasource_id": "pg-rna-public",
  "driver": "postgres",
  "idle_connections": 3,
  "keep_alive_sec": 30,
  "masked_dsn": "postgres://reader:%2A%2A%2A@hh-pgsql-public.ebi.ac.uk:5432/pfmegrnargs",
  "max_pool_size": 10,
  "min_pool_size": 3,
  "open_connections": 3,
  "ping_latency_ms": 11,
  "status": "registered_and_warmed",
  "success": true
}
```

#### Supported `jdbc_url` Formats

| Database Engine | `driver` | Example `jdbc_url` |Bind Syntax |
| :--- | :--- | :--- | :---: |
| **PostgreSQL / AlloyDB** | `postgres` | `jdbc:postgresql://10.240.0.3:5432/billing` | `$[param]` $\rightarrow$ `$1` |
| **MySQL / MariaDB** | `mysql` | `jdbc:mysql://10.240.0.4:3306/orders?tls=true` | `$[param]` $\rightarrow$ `?` |
| **Microsoft SQL Server** | `sqlserver` | `jdbc:sqlserver://10.240.0.5:1433;databaseName=crm` | `$[param]` $\rightarrow$ `@p1` |
| **Oracle Database** | `oracle` | `jdbc:oracle:thin:@//10.240.0.6:1521/ORCLPDB1` | `$[param]` $\rightarrow$ `:1` |
| **SQLite (In-Memory / File)** | `sqlite` | `jdbc:sqlite::memory:` | `$[param]` $\rightarrow$ `?` |

---

### Step 2 — Consume the Warm Pool in Any Apigee Proxy (`jdbc-policy-callout-v1`)

API consumers call standard REST endpoints without passing any database identifiers. Inside Apigee, your proxy reads the target `datasource_id` from the bundled **`jdbc.properties` PropertySet** (`Resources -> properties -> jdbc.properties`) and executes a single `ServiceCallout`.

![Step 2 Sequence](docs/images/jdbc-runtime-flow-sequence.svg)

#### 1. PropertySet (`apigee/jdbc.properties`)
```properties
datasource_id=enterprise-sqlite
external_datasource_id=pg-rna-public
private_vm_datasource_id=private-vm-postgres
variable_prefix=jdbc
max_rows=100
```

#### 2. Drop-In Apigee `ServiceCallout` Policy (`SC-JDBC-Query`)
```xml
<ServiceCallout async="false" continueOnError="true" enabled="true" name="SC-JDBC-Query">
  <Request clearPayload="true" variable="jdbc.callout.request">
    <IgnoreUnresolvedVariables>true</IgnoreUnresolvedVariables>
    <Set>
      <Headers>
        <Header name="Content-Type">application/json</Header>
      </Headers>
      <Payload contentType="application/json">{
        "datasource_id": "{propertyset.jdbc.external_datasource_id}",
        "variable_prefix": "{propertyset.jdbc.variable_prefix}",
        "max_rows": 100,
        "fail_if_empty": true,
        "use_transaction": false,
        "sql": "SELECT urs AS upi, urs_taxid AS crc64, rna_type, description AS seq_short FROM rnc_rna_precomputed WHERE urs = $[upi] LIMIT 1",
        "params": {
          "upi": "{request.queryparam.upi}"
        }
      }</Payload>
      <Verb>POST</Verb>
    </Set>
  </Request>
  <Response>jdbc.callout.response</Response>
  <Timeout>10000</Timeout>
  <HTTPTargetConnection>
    <Authentication>
      <GoogleIDToken>
        <Audience>jdbc-policy-service</Audience>
      </GoogleIDToken>
    </Authentication>
    <Properties>
      <Property name="keepalive.timeout.millis">60000</Property>
      <Property name="success.codes">2xx,4xx,5xx</Property>
    </Properties>
    <URL>https://jdbc-policy-connector-xxxx.a.run.app/v1/jdbc/query</URL>
  </HTTPTargetConnection>
</ServiceCallout>
```

---

## Connector API Reference

### 1. Datasource Pool Management (`/v1/jdbc-admin/datasources`)

| Method & Path | Description |
| :--- | :--- |
| `GET /v1/jdbc-admin/datasources` | Lists all active datasource pools, open/idle socket counts, and Keep-Alive ping latency. |
| `GET /v1/jdbc-admin/datasources?id=<id>` | Returns health and masked connection details (`********`) for a specific datasource. |
| `POST /v1/jdbc-admin/datasources` | Encrypts the schema in `jdbc-datasources-kvm`, pre-opens `min_pool_size` sockets, and starts the Keep-Alive loop. |
| `DELETE /v1/jdbc-admin/datasources?id=<id>` | Deletes the KVM entry, stops the Keep-Alive loop, and drains all pooled sockets. |

### 2. SQL Execution (`/v1/jdbc/query`, `/v1/jdbc/execute`, `/v1/jdbc/procedure`)

#### Request Parameters (`POST` JSON)

| Field | Type | Default | Description |
| :--- | :---: | :---: | :--- |
| `datasource_id` | `string` | *(Required)* | Target datasource ID resolved from `{propertyset.jdbc.datasource_id}`. |
| `sql` | `string` | *(Required)* | Parameterized SQL statement (`SELECT`, `WITH`, `INSERT`, `UPDATE`, `DELETE`, `CALL`, `EXEC`) using `$[param]` or `${param}` placeholders. |
| `params` | `object` | `{}` | Key-value map of bind parameters extracted from Apigee request headers, query params, or JSON body. |
| `variable_prefix` | `string` | `"jdbc"` | Prefix for generated Apigee context variables (`jdbc.row_count`, `jdbc.truncated`, `jdbc.<col>`). |
| `max_rows` | `int` | `100` | Maximum rows to stream from a `SELECT` cursor before closing the cursor and setting `"truncated": true` (hard ceiling: `5000`). |
| `fail_if_empty` | `bool` | `false` | When `true`, returns `404 Not Found` (`EMPTY_RESULT_SET`) if a `SELECT` returns `0` rows. |
| `use_transaction` | `bool` | `false` | Wraps DML (`INSERT`/`UPDATE`/`DELETE`) in `BEGIN` $\rightarrow$ `COMMIT` with automatic `ROLLBACK` on error. |
| `timeout_ms` | `int` | `5000` | Per-query execution deadline in milliseconds (returns `504 Gateway Timeout` if exceeded). |

#### Response Schema (`200 OK` JSON)

```json
{
  "apigee_policy": "SC-JDBC-Policy-Callout",
  "propertyset_Selector": "jdbc.properties",
  "datasource_id": "pg-rna-public",
  "driver": "postgres",
  "operation_type": "SELECT",
  "success": "true",
  "execution_time_ms": "6",
  "extracted_context_variables": {
    "jdbc.row_count": "3",
    "jdbc.truncated": "true",
    "jdbc.last_insert_id": "",
    "jdbc.upi": "URS0000A345E3",
    "jdbc.crc64": "URS0000A345E3_77133",
    "jdbc.seq_short": "uncultured bacterium partial 16S ribosomal RNA"
  },
  "injected_backend_headers": {
    "X-JDBC-Datasource": "pg-rna-public",
    "X-JDBC-Driver": "postgres",
    "X-JDBC-Row-Count": "3"
  },
  "rows": [
    {
      "crc64": "URS0000A345E3_77133",
      "rna_type": "rRNA",
      "seq_short": "uncultured bacterium partial 16S ribosomal RNA",
      "upi": "URS0000A345E3"
    },
    {
      "crc64": "URS0000A345EA_198431",
      "rna_type": "rRNA",
      "seq_short": "uncultured prokaryote partial 16S ribosomal RNA",
      "upi": "URS0000A345EA"
    },
    {
      "crc64": "URS0000A345EE_77133",
      "rna_type": "rRNA",
      "seq_short": "uncultured bacterium partial 16S ribosomal RNA",
      "upi": "URS0000A345EE"
    }
  ]
}
```

---

## Handling Large `SELECT` Payloads & Multi-Row Result Sets

1. **Streaming Cursor Cap (`max_rows` + `truncated: true`):**
   - `src/executor.go` reads from the database cursor row-by-row up to `max_rows`. If row `max_rows + 1` exists, it immediately closes the cursor (preventing Cloud Run OOM and protecting Apigee's 10 MB message buffer) and sets `"truncated": true` (`jdbc.truncated = "true"`).
2. **Full Multi-Row Array Extraction (`$.rows` via `type="nodeset"`):**
   - `EV-JDBC-Context-Variables` extracts `$.first_row.<col>` into scalar variables (`jdbc.<col>`) AND extracts `$.rows` as `type="nodeset"` (`{jdbc.rows_json}`) so multi-row JSON arrays are preserved without looping policies.
3. **Parameterized Pagination (`LIMIT $[limit] OFFSET $[offset]`):**
   - Bind `{request.queryparam.limit}` and `{request.queryparam.offset}` directly as prepared parameters.
4. **Pooled HTTP `gzip` Compression:**
   - Responses are compressed via a `sync.Pool` of `gzip.BestSpeed` writers when `Accept-Encoding: gzip` is sent, reducing multi-row payload sizes by ~85%.

---

## Automated Verification Suite (`./test.sh`) & Private VPC Sandbox

Run `./test.sh` to execute **9 automated tests** with `jq` assertions against both the built-in SQLite pool and a live external PostgreSQL database (`hh-pgsql-public.ebi.ac.uk:5432`):

| # | Verification Check | Endpoint | Expected Outcome |
| :---: | :--- | :--- | :--- |
| **1** | List Initial Warm Pool (`enterprise-sqlite`) | `GET /v1/jdbc-admin/datasources` | `200 OK` (`open_connections >= 1`, `healthy: true`) |
| **2** | Onboard Live External PostgreSQL (`pg-rna-public`) | `POST /v1/jdbc-admin/datasources` | `200 OK` (`status: "registered_and_warmed"`, `open_connections: 3`) |
| **3** | Verify Pool Health & Credential Masking | `GET /v1/jdbc-admin/datasources?id=pg-rna-public` | `200 OK` (`healthy: true`, password masked with `***`) |
| **4** | Admin Conditional Guard (Missing `datasource_id`) | `POST /v1/jdbc-admin/datasources` | `400 Bad Request` (`MISSING_DATASOURCE_ID`) |
| **5** | Single-Row `SELECT` via `{propertyset.jdbc.datasource_id}` | `GET /v1/jdbc-policy/subscribers?id=1001` | `200 OK` (`jdbc.full_name: "Elena Rostova"`, `rows` length `1`) |
| **6** | Empty Result Set Guard (`fail_if_empty: true`) | `GET /v1/jdbc-policy/subscribers?id=9999` | `404 Not Found` (`EMPTY_RESULT_SET`) |
| **7** | Transactional DML `INSERT` (`use_transaction: true`) | `POST /v1/jdbc-policy/orders` | `200 OK` (`operation_type: "DML"`, `jdbc.row_count: "1"`) |
| **8** | Live External PostgreSQL Single-Row `SELECT` | `GET /v1/jdbc-policy/rna?upi=URS0000000001` | `200 OK` (`datasource_id: "pg-rna-public"`, `3–11 ms` warm query) |
| **9** | Live External PostgreSQL Multi-Row `SELECT` + Cap | `GET /v1/jdbc-policy/rna-search?rna_type=rRNA` | `200 OK` (`rows` length `3`, `jdbc.truncated: "true"`) |

### Optional: Private VPC PostgreSQL Container VM (`04-optional-test-db-vm.sh`)
To test **Cloud Run Direct VPC Egress** against a private `10.x.x.x:5432` database (simulating an on-premises database over Cloud VPN / Interconnect):

```bash
# Provision private GCE VM (--no-address), attach Direct VPC Egress, onboard & query via Apigee
./scripts/04-optional-test-db-vm.sh --create

# Tear down the VM, firewall rule, and datasource when finished
./scripts/04-optional-test-db-vm.sh --delete
```

---

## Performance Benchmarks (`europe-west2`)

Measured across **650 sequential and concurrent requests (`10` workers)** on a single warm Cloud Run instance (`1 vCPU`, `512 MiB` RAM, `min_pool_size: 3`):

| Workload | DB Execution (`p50`) | Direct Cloud Run (`p50`) | End-to-End Apigee X (`p50`) | Sustained Throughput (`1` Instance) |
| :--- | :---: | :---: | :---: | :---: |
| **Warm SQLite `SELECT`** | `0.0 ms` | `17.1 ms` | `60.5 ms` | **`213.1 req/s`** (Direct) / **`129.3 req/s`** (Apigee) |
| **Warm External PostgreSQL `SELECT`** | `3.0 ms` | `21.1 ms` | `62.4 ms` | **`188.0 req/s`** (Direct) / **`117.0 req/s`** (Apigee) |
| **Warm External PostgreSQL Multi-Row `SELECT`** | `3.0 ms` | `20.2 ms` | `55.8 ms` | **`188.0 req/s`** (Direct) / **`102.1 req/s`** (Apigee) |

---

## Production (`PRD`) Deployment Checklist

1. **Protect the Admin Proxy (`jdbc-admin-v1`):**
   - Attach an Apigee `OAuthV2` (`VerifyAccessToken`) or `VerifyAPIKey` policy to `jdbc-admin-v1` (or restrict `/v1/jdbc-admin` in Cloud Armor) so only authorized platform administrators or CI/CD pipelines can mutate datasources.
2. **Restrict Cloud Run Network Ingress (`--ingress=internal`):**
   - When your Apigee X organization uses Peered VPC or Private Service Connect (PSC) egress, set `--ingress="internal"` in `scripts/02-deploy-cloudrun.sh`.
3. **Right-Size Database Connection Limits (`max-instances` $\times$ `max_pool_size`):**
   - Ensure `Cloud Run --max-instances` $\times$ `max_pool_size` remains below your database's `max_connections` budget (e.g., `5` instances $\times$ `15` max connections = `75` total connections).
4. **Cloud Logging & Monitoring Alerts:**
   - Every query emits a single-line structured JSON log (`message: "jdbc_query_executed"`, `datasource_id`, `driver`, `operation_type`, `row_count`, `truncated`, `execution_time_ms`, `http_status`) automatically indexed by **Google Cloud Logging**.

---

## Repository Structure

```text
apigee-cloudrun-jdbc-policy/
├── README.md                                  # Product documentation, API reference & PRD guide
├── deploy.sh                                  # One-command end-to-end deployment orchestrator
├── test.sh                                    # 9-test verification suite with jq assertions
├── colab/
│   └── deploy_cloudrun_apigee.ipynb           # Interactive Google Colab / Colab Enterprise UI
├── docs/
│   └── images/
│       ├── jdbc-architecture-overview.svg     # System architecture diagram
│       ├── jdbc-admin-flow-sequence.svg       # Step 1 sequence diagram (Admin & Keep-Alive Pool)
│       └── jdbc-runtime-flow-sequence.svg     # Step 2 sequence diagram (PropertySet & ServiceCallout)
├── src/
│   ├── Dockerfile                             # Multi-stage Go 1.22 -> distroless/static:nonroot
│   ├── go.mod                                 # Pure-Go SQL drivers (pgx/v5, mysql, mssqldb, go-ora/v2, sqlite)
│   ├── main.go                                # HTTP handlers, pooled gzip middleware & structured Cloud Logging
│   ├── pool.go                                # Persistent *sql.DB pools, *sql.Stmt cache, Keep-Alive & KVM sync
│   ├── executor.go                            # Prepared SQL compiler ($[var] -> $1/?/@p1/:1) & cursor executor
│   ├── seed_db.go                             # Built-in seeded SQLite database for zero-dependency verification
│   └── datasources.json                       # Bootstrap datasource configuration
├── apigee/
│   ├── jdbc.properties                        # Apigee PropertySet mapping proxy routes to datasource_id
│   ├── jdbc-admin-proxy.template.yaml         # Apigee Templater YAML for jdbc-admin-v1
│   └── jdbc-policy-proxy.template.yaml        # Apigee Templater YAML for jdbc-policy-callout-v1
└── scripts/
    ├── 00-env.sh                              # Shared configuration & dynamic Apigee topology discovery
    ├── 01-setup-gcp.sh                        # GCP APIs, Runtime SA, Invoker SA & IAM bindings
    ├── 02-deploy-cloudrun.sh                  # Private Cloud Run deployment + Custom Audience + Direct VPC Egress
    ├── 03-deploy-apigee.sh                    # Encrypted KVM + PropertySet upload + proxy bundle deployment
    └── 04-optional-test-db-vm.sh              # Optional Private VPC PostgreSQL Container VM (--create / --delete)
```
