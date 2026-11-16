package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "github.com/sijms/go-ora/v2"
	_ "modernc.org/sqlite"
)

// DatasourceConfig represents a structured database connection and pool definition.
type DatasourceConfig struct {
	DatasourceID    string            `json:"datasource_id"`
	Description     string            `json:"description,omitempty"`
	Driver          string            `json:"driver"`                     // postgres, mysql, sqlserver, oracle, sqlite
	JDBCURL         string            `json:"jdbc_url,omitempty"`         // e.g. jdbc:postgresql://host:5432/db
	DSN             string            `json:"dsn,omitempty"`              // optional raw driver DSN
	Host            string            `json:"host,omitempty"`             // optional structured host
	Port            int               `json:"port,omitempty"`             // optional structured port
	Database        string            `json:"database,omitempty"`         // optional structured database/service
	Username        string            `json:"username,omitempty"`         // separate username (safe with special chars)
	Password        string            `json:"password,omitempty"`         // separate password (masked on GET)
	Properties      map[string]string `json:"properties,omitempty"`       // driver properties (sslmode, connect_timeout, etc.)
	MinPoolSize     int               `json:"min_pool_size,omitempty"`    // default 3 (warm idle connections)
	MaxPoolSize     int               `json:"max_pool_size,omitempty"`    // default 15 (max open connections)
	MaxIdleTimeSec  int               `json:"max_idle_time_sec,omitempty"`
	MaxLifetimeSec  int               `json:"max_lifetime_sec,omitempty"`
	KeepAliveSec    int               `json:"keep_alive_sec,omitempty"`   // background SELECT 1 heartbeat interval (default 30s)
	QueryTimeoutMs  int               `json:"query_timeout_ms,omitempty"` // per-query timeout (default 5000ms)
}

// DatasourcePool wraps a persistent *sql.DB connection pool, prepared-statement cache,
// and its background keep-alive heartbeat.
type DatasourcePool struct {
	Config            DatasourceConfig
	SQLDriverName     string
	ResolvedDSN       string
	DB                *sql.DB
	stmtCache         sync.Map // map[string]*sql.Stmt for server-side prepared statements
	cancelKeepAlive   context.CancelFunc
	mu                sync.RWMutex
	lastKeepAliveAt   time.Time
	lastPingLatencyMs int64
	lastPingError     string
}

// DatasourceStatus summarizes a datasource and its live connection pool telemetry.
type DatasourceStatus struct {
	DatasourceID      string            `json:"datasource_id"`
	Description       string            `json:"description,omitempty"`
	Driver            string            `json:"driver"`
	JDBCURL           string            `json:"jdbc_url,omitempty"`
	MaskedDSN         string            `json:"masked_dsn"`
	Username          string            `json:"username,omitempty"`
	MinPoolSize       int               `json:"min_pool_size"`
	MaxPoolSize       int               `json:"max_pool_size"`
	KeepAliveSec      int               `json:"keep_alive_sec"`
	QueryTimeoutMs    int               `json:"query_timeout_ms"`
	Properties        map[string]string `json:"properties,omitempty"`
	OpenConnections   int               `json:"open_connections"`
	InUse             int               `json:"in_use"`
	Idle              int               `json:"idle"`
	WaitCount         int64             `json:"wait_count"`
	WaitDurationMs    int64             `json:"wait_duration_ms"`
	LastKeepAliveAt   string            `json:"last_keepalive_at,omitempty"`
	LastPingLatencyMs int64             `json:"last_ping_latency_ms"`
	Healthy           bool              `json:"healthy"`
}

// DatasourceRegistry manages all active database connection pools.
type DatasourceRegistry struct {
	mu           sync.RWMutex
	pools        map[string]*DatasourcePool
	hydrateMu    sync.Mutex // singleflight guard for lazy KVM hydration
	apigeeOrg    string
	apigeeEnv    string
	apigeeKVM    string
	tokenMu      sync.Mutex
	cachedToken  string
	tokenExpires time.Time
}

func NewDatasourceRegistry() *DatasourceRegistry {
	return &DatasourceRegistry{
		pools:     make(map[string]*DatasourcePool),
		apigeeOrg: strings.TrimSpace(os.Getenv("APIGEE_ORG")),
		apigeeEnv: strings.TrimSpace(os.Getenv("APIGEE_ENV")),
		apigeeKVM: strings.TrimSpace(os.Getenv("APIGEE_KVM_NAME")),
	}
}

func normalizeDefaults(cfg *DatasourceConfig) {
	cfg.DatasourceID = strings.TrimSpace(cfg.DatasourceID)
	cfg.Driver = strings.ToLower(strings.TrimSpace(cfg.Driver))
	if cfg.MinPoolSize <= 0 {
		cfg.MinPoolSize = 3
	}
	if cfg.MaxPoolSize <= 0 {
		cfg.MaxPoolSize = 15
	}
	if cfg.MinPoolSize > cfg.MaxPoolSize {
		cfg.MinPoolSize = cfg.MaxPoolSize
	}
	if cfg.MaxIdleTimeSec <= 0 {
		cfg.MaxIdleTimeSec = 300
	}
	if cfg.MaxLifetimeSec <= 0 {
		cfg.MaxLifetimeSec = 900
	}
	if cfg.KeepAliveSec <= 0 {
		cfg.KeepAliveSec = 30
	}
	if cfg.QueryTimeoutMs <= 0 {
		cfg.QueryTimeoutMs = 5000
	}
}

// BuildDriverAndDSN translates structured JDBC connection properties (or raw DSN)
// into a Go database/sql driver name and properly escaped connection string.
func BuildDriverAndDSN(cfg DatasourceConfig) (string, string, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	rawJDBC := strings.TrimSpace(cfg.JDBCURL)

	// Infer driver from jdbc:... prefix if driver was omitted or set to generic "jdbc"
	if strings.HasPrefix(rawJDBC, "jdbc:") {
		trimmed := strings.TrimPrefix(rawJDBC, "jdbc:")
		switch {
		case strings.HasPrefix(trimmed, "postgresql:") || strings.HasPrefix(trimmed, "postgres:"):
			driver = "postgres"
		case strings.HasPrefix(trimmed, "mysql:") || strings.HasPrefix(trimmed, "mariadb:"):
			driver = "mysql"
		case strings.HasPrefix(trimmed, "sqlserver:") || strings.HasPrefix(trimmed, "microsoft:sqlserver:"):
			driver = "sqlserver"
		case strings.HasPrefix(trimmed, "oracle:"):
			driver = "oracle"
		case strings.HasPrefix(trimmed, "sqlite:"):
			driver = "sqlite"
		}
	}

	switch driver {
	case "postgres", "postgresql", "pgx":
		dsn, err := buildPostgresDSN(cfg)
		return "pgx", dsn, err
	case "mysql", "mariadb":
		dsn, err := buildMySQLDSN(cfg)
		return "mysql", dsn, err
	case "sqlserver", "mssql":
		dsn, err := buildSQLServerDSN(cfg)
		return "sqlserver", dsn, err
	case "oracle", "ora":
		dsn, err := buildOracleDSN(cfg)
		return "oracle", dsn, err
	case "sqlite", "sqlite3":
		dsn := buildSQLiteDSN(cfg)
		return "sqlite", dsn, nil
	default:
		return "", "", fmt.Errorf("unsupported driver %q (supported: postgres, mysql, sqlserver, oracle, sqlite)", cfg.Driver)
	}
}

func buildPostgresDSN(cfg DatasourceConfig) (string, error) {
	if cfg.DSN != "" && !strings.HasPrefix(cfg.DSN, "jdbc:") {
		return injectUserPassInURL(cfg.DSN, cfg.Username, cfg.Password, cfg.Properties)
	}
	raw := strings.TrimSpace(cfg.JDBCURL)
	if raw == "" && cfg.Host != "" {
		port := cfg.Port
		if port == 0 {
			port = 5432
		}
		raw = fmt.Sprintf("postgres://%s:%d/%s", cfg.Host, port, cfg.Database)
	}
	raw = strings.TrimPrefix(raw, "jdbc:")
	if strings.HasPrefix(raw, "postgresql://") {
		raw = "postgres://" + strings.TrimPrefix(raw, "postgresql://")
	}
	if !strings.HasPrefix(raw, "postgres://") {
		return "", errors.New("postgres jdbc_url must start with jdbc:postgresql:// or postgres://")
	}
	return injectUserPassInURL(raw, cfg.Username, cfg.Password, cfg.Properties)
}

func buildMySQLDSN(cfg DatasourceConfig) (string, error) {
	if cfg.DSN != "" && !strings.HasPrefix(cfg.DSN, "jdbc:") && !strings.Contains(cfg.DSN, "://") {
		return cfg.DSN, nil
	}
	raw := strings.TrimSpace(cfg.JDBCURL)
	if raw == "" {
		raw = strings.TrimSpace(cfg.DSN)
	}
	raw = strings.TrimPrefix(raw, "jdbc:")
	raw = strings.TrimPrefix(raw, "mariadb://")
	raw = strings.TrimPrefix(raw, "mysql://")

	hostPort := cfg.Host
	dbName := cfg.Database
	queryProps := url.Values{}
	for k, v := range cfg.Properties {
		queryProps.Set(k, v)
	}

	if raw != "" {
		u, err := url.Parse("mysql://" + raw)
		if err != nil {
			return "", fmt.Errorf("invalid mysql jdbc_url: %w", err)
		}
		hostPort = u.Host
		dbName = strings.TrimPrefix(u.Path, "/")
		if u.User != nil && cfg.Username == "" {
			cfg.Username = u.User.Username()
			if p, ok := u.User.Password(); ok && cfg.Password == "" {
				cfg.Password = p
			}
		}
		for k, vals := range u.Query() {
			if len(vals) > 0 && queryProps.Get(k) == "" {
				queryProps.Set(k, vals[0])
			}
		}
	}
	if hostPort == "" {
		return "", errors.New("mysql host is required")
	}
	if !strings.Contains(hostPort, ":") {
		port := cfg.Port
		if port == 0 {
			port = 3306
		}
		hostPort = fmt.Sprintf("%s:%d", hostPort, port)
	}
	if queryProps.Get("parseTime") == "" {
		queryProps.Set("parseTime", "true")
	}
	userPart := cfg.Username
	if cfg.Password != "" {
		userPart = fmt.Sprintf("%s:%s", cfg.Username, cfg.Password)
	}
	q := queryProps.Encode()
	if q != "" {
		return fmt.Sprintf("%s@tcp(%s)/%s?%s", userPart, hostPort, dbName, q), nil
	}
	return fmt.Sprintf("%s@tcp(%s)/%s", userPart, hostPort, dbName), nil
}

func buildSQLServerDSN(cfg DatasourceConfig) (string, error) {
	if cfg.DSN != "" && strings.HasPrefix(cfg.DSN, "sqlserver://") {
		return injectUserPassInURL(cfg.DSN, cfg.Username, cfg.Password, cfg.Properties)
	}
	raw := strings.TrimPrefix(strings.TrimSpace(cfg.JDBCURL), "jdbc:")
	// Handle jdbc:sqlserver://host:1433;databaseName=mydb
	hostPort := cfg.Host
	params := url.Values{}
	if cfg.Database != "" {
		params.Set("database", cfg.Database)
	}
	for k, v := range cfg.Properties {
		params.Set(k, v)
	}
	if strings.HasPrefix(raw, "sqlserver://") {
		rest := strings.TrimPrefix(raw, "sqlserver://")
		parts := strings.Split(rest, ";")
		hostPort = strings.TrimSuffix(parts[0], "/")
		for _, p := range parts[1:] {
			kv := strings.SplitN(p, "=", 2)
			if len(kv) == 2 {
				k := strings.TrimSpace(kv[0])
				v := strings.TrimSpace(kv[1])
				if strings.EqualFold(k, "databaseName") || strings.EqualFold(k, "database") {
					params.Set("database", v)
				} else {
					params.Set(k, v)
				}
			}
		}
	}
	if hostPort == "" {
		return "", errors.New("sqlserver host is required")
	}
	u := &url.URL{
		Scheme:   "sqlserver",
		Host:     hostPort,
		RawQuery: params.Encode(),
	}
	if cfg.Username != "" {
		u.User = url.UserPassword(cfg.Username, cfg.Password)
	}
	return u.String(), nil
}

func buildOracleDSN(cfg DatasourceConfig) (string, error) {
	if cfg.DSN != "" && strings.HasPrefix(cfg.DSN, "oracle://") {
		return injectUserPassInURL(cfg.DSN, cfg.Username, cfg.Password, cfg.Properties)
	}
	raw := strings.TrimSpace(cfg.JDBCURL)
	// Handle jdbc:oracle:thin:@//host:1521/service or jdbc:oracle:thin:@host:1521:SID
	hostPort := cfg.Host
	service := cfg.Database
	if strings.HasPrefix(raw, "jdbc:oracle:") {
		idx := strings.Index(raw, "@")
		if idx != -1 {
			afterAt := strings.TrimPrefix(raw[idx+1:], "//")
			afterAt = strings.ReplaceAll(afterAt, ":", "/")
			parts := strings.Split(afterAt, "/")
			if len(parts) >= 3 {
				hostPort = parts[0] + ":" + parts[1]
				service = strings.Join(parts[2:], "/")
			} else if len(parts) == 2 {
				hostPort = parts[0]
				service = parts[1]
			}
		}
	}
	if hostPort == "" {
		return "", errors.New("oracle host:port is required")
	}
	if !strings.Contains(hostPort, ":") {
		port := cfg.Port
		if port == 0 {
			port = 1521
		}
		hostPort = fmt.Sprintf("%s:%d", hostPort, port)
	}
	params := url.Values{}
	for k, v := range cfg.Properties {
		params.Set(k, v)
	}
	u := &url.URL{
		Scheme:   "oracle",
		Host:     hostPort,
		Path:     "/" + strings.TrimPrefix(service, "/"),
		RawQuery: params.Encode(),
	}
	if cfg.Username != "" {
		u.User = url.UserPassword(cfg.Username, cfg.Password)
	}
	return u.String(), nil
}

func buildSQLiteDSN(cfg DatasourceConfig) string {
	raw := strings.TrimSpace(cfg.DSN)
	if raw == "" {
		raw = strings.TrimSpace(cfg.JDBCURL)
	}
	raw = strings.TrimPrefix(raw, "jdbc:sqlite:")
	if raw == "" || raw == ":memory:" || strings.Contains(raw, "mode=memory") {
		// Use Cloud Run's in-memory tmpfs (/tmp) so connection rotation at ConnMaxLifetime
		// never wipes the seeded SQLite tables while still running 100% in RAM.
		// Synchronous=NORMAL + WAL + memory temp_store provides maximum concurrency & sub-ms DML.
		safeID := strings.ReplaceAll(cfg.DatasourceID, "-", "_")
		if safeID == "" {
			safeID = "default"
		}
		return fmt.Sprintf("file:/tmp/%s.sqlite?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=temp_store(MEMORY)", safeID)
	}
	return raw
}

func injectUserPassInURL(rawURL, username, password string, props map[string]string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if username != "" {
		u.User = url.UserPassword(username, password)
	} else if u.User != nil && password != "" {
		u.User = url.UserPassword(u.User.Username(), password)
	}
	q := u.Query()
	for k, v := range props {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func maskDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			u.User = url.UserPassword(u.User.Username(), "***")
		}
		return u.String()
	}
	// Mask mysql user:pass@tcp(...)
	if atIdx := strings.Index(dsn, "@tcp("); atIdx != -1 {
		userPass := dsn[:atIdx]
		if colonIdx := strings.Index(userPass, ":"); colonIdx != -1 {
			return userPass[:colonIdx] + ":***" + dsn[atIdx:]
		}
	}
	return dsn
}

// Upsert validates the connection, pre-warms min_pool_size connections, starts
// the background Keep-Alive heartbeat goroutine, and registers the pool.
func (r *DatasourceRegistry) Upsert(ctx context.Context, cfg DatasourceConfig) (*DatasourceStatus, error) {
	normalizeDefaults(&cfg)
	if cfg.DatasourceID == "" {
		return nil, errors.New("datasource_id is required")
	}

	sqlDriver, dsn, err := BuildDriverAndDSN(cfg)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(sqlDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open failed: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxPoolSize)
	db.SetMaxIdleConns(cfg.MinPoolSize)
	db.SetConnMaxIdleTime(time.Duration(cfg.MaxIdleTimeSec) * time.Second)
	db.SetConnMaxLifetime(time.Duration(cfg.MaxLifetimeSec) * time.Second)

	// Pre-warm connections and measure initial ping latency
	t0 := time.Now()
	pingCtx, cancelPing := context.WithTimeout(ctx, time.Duration(cfg.QueryTimeoutMs)*time.Millisecond)
	defer cancelPing()

	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database connection test (PingContext) failed: %w", err)
	}
	pingLatency := time.Since(t0).Milliseconds()

	// If built-in sqlite, seed the enterprise tables automatically
	if sqlDriver == "sqlite" {
		if err := SeedEnterpriseSQLite(pingCtx, db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed seeding built-in sqlite database: %w", err)
		}
	}

	// Pre-open MinPoolSize concurrent connections in parallel so the pool is immediately warm
	type warmResult struct {
		conn *sql.Conn
	}
	warmCh := make(chan *sql.Conn, cfg.MinPoolSize)
	var warmWG sync.WaitGroup
	for i := 0; i < cfg.MinPoolSize; i++ {
		warmWG.Add(1)
		go func() {
			defer warmWG.Done()
			if c, err := db.Conn(pingCtx); err == nil {
				// Execute a lightweight ping on this specific socket so the driver actually opens the TCP/TLS connection
				if err := c.PingContext(pingCtx); err == nil {
					warmCh <- c
					return
				}
				_ = c.Close()
			}
		}()
	}
	warmWG.Wait()
	close(warmCh)
	for c := range warmCh {
		_ = c.Close() // Returns the verified open socket directly into the idle pool
	}

	kaCtx, cancelKA := context.WithCancel(context.Background())
	pool := &DatasourcePool{
		Config:            cfg,
		SQLDriverName:     sqlDriver,
		ResolvedDSN:       dsn,
		DB:                db,
		cancelKeepAlive:   cancelKA,
		lastKeepAliveAt:   time.Now().UTC(),
		lastPingLatencyMs: pingLatency,
	}

	go pool.runKeepAliveLoop(kaCtx)

	r.mu.Lock()
	oldPool, existed := r.pools[cfg.DatasourceID]
	r.pools[cfg.DatasourceID] = pool
	r.mu.Unlock()

	if existed && oldPool != nil {
		oldPool.Close()
	}

	status := pool.Status()
	return &status, nil
}

// runKeepAliveLoop periodically pings the database so idle connections in the pool
// are never dropped by on-premises firewalls, VPN tunnels, or DB wait_timeout timers.
func (p *DatasourcePool) runKeepAliveLoop(ctx context.Context) {
	interval := time.Duration(p.Config.KeepAliveSec) * time.Second
	if interval < 5*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t0 := time.Now()
			pCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			// Ping across MinPoolSize concurrent connections so every warm idle socket stays active
			heldConns := make([]*sql.Conn, 0, p.Config.MinPoolSize)
			var pingErr error
			for i := 0; i < p.Config.MinPoolSize; i++ {
				c, err := p.DB.Conn(pCtx)
				if err != nil {
					if pingErr == nil {
						pingErr = err
					}
					break
				}
				if err := c.PingContext(pCtx); err != nil && pingErr == nil {
					pingErr = err
				}
				heldConns = append(heldConns, c)
			}
			for _, c := range heldConns {
				_ = c.Close()
			}
			cancel()
			lat := time.Since(t0).Milliseconds()

			p.mu.Lock()
			p.lastKeepAliveAt = time.Now().UTC()
			p.lastPingLatencyMs = lat
			if pingErr != nil {
				p.lastPingError = pingErr.Error()
			} else {
				p.lastPingError = ""
			}
			p.mu.Unlock()
		}
	}
}

// GetOrPrepareStmt returns a cached *sql.Stmt for the given SQL query, or prepares and caches it.
func (p *DatasourcePool) GetOrPrepareStmt(ctx context.Context, query string) (*sql.Stmt, error) {
	if cached, ok := p.stmtCache.Load(query); ok {
		return cached.(*sql.Stmt), nil
	}
	stmt, err := p.DB.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	actual, loaded := p.stmtCache.LoadOrStore(query, stmt)
	if loaded {
		_ = stmt.Close()
		return actual.(*sql.Stmt), nil
	}
	return stmt, nil
}

// EvictStmt removes and closes a cached *sql.Stmt (e.g., if a table schema changed or connection reset).
func (p *DatasourcePool) EvictStmt(query string) {
	if val, loaded := p.stmtCache.LoadAndDelete(query); loaded {
		_ = val.(*sql.Stmt).Close()
	}
}

// Close stops the background keep-alive ticker, closes cached prepared statements, and drains all pooled connections.
func (p *DatasourcePool) Close() {
	if p.cancelKeepAlive != nil {
		p.cancelKeepAlive()
	}
	p.stmtCache.Range(func(key, value any) bool {
		if stmt, ok := value.(*sql.Stmt); ok {
			_ = stmt.Close()
		}
		p.stmtCache.Delete(key)
		return true
	})
	if p.DB != nil {
		_ = p.DB.Close()
	}
}

func (p *DatasourcePool) Status() DatasourceStatus {
	stats := p.DB.Stats()
	p.mu.RLock()
	lastKA := p.lastKeepAliveAt
	lastLat := p.lastPingLatencyMs
	lastErr := p.lastPingError
	p.mu.RUnlock()

	kaStr := ""
	if !lastKA.IsZero() {
		kaStr = lastKA.Format(time.RFC3339)
	}
	return DatasourceStatus{
		DatasourceID:      p.Config.DatasourceID,
		Description:       p.Config.Description,
		Driver:            p.Config.Driver,
		JDBCURL:           p.Config.JDBCURL,
		MaskedDSN:         maskDSN(p.ResolvedDSN),
		Username:          p.Config.Username,
		MinPoolSize:       p.Config.MinPoolSize,
		MaxPoolSize:       p.Config.MaxPoolSize,
		KeepAliveSec:      p.Config.KeepAliveSec,
		QueryTimeoutMs:    p.Config.QueryTimeoutMs,
		Properties:        p.Config.Properties,
		OpenConnections:   stats.OpenConnections,
		InUse:             stats.InUse,
		Idle:              stats.Idle,
		WaitCount:         stats.WaitCount,
		WaitDurationMs:    stats.WaitDuration.Milliseconds(),
		LastKeepAliveAt:   kaStr,
		LastPingLatencyMs: lastLat,
		Healthy:           lastErr == "",
	}
}

// Resolve returns an active DatasourcePool by ID. If not in local memory yet
// (e.g. registered on a sibling instance), it attempts a lazy sync from Apigee's Encrypted KVM.
func (r *DatasourceRegistry) Resolve(ctx context.Context, datasourceID string) (*DatasourcePool, error) {
	id := strings.TrimSpace(datasourceID)
	if id == "" {
		return nil, errors.New("datasource_id is required (no global default datasource is configured)")
	}
	r.mu.RLock()
	pool, ok := r.pools[id]
	r.mu.RUnlock()
	if ok {
		return pool, nil
	}

	// Attempt automatic self-hydration from Apigee Encrypted KVM with singleflight lock
	r.hydrateMu.Lock()
	defer r.hydrateMu.Unlock()

	// Re-check under hydrateMu in case another goroutine just hydrated it
	r.mu.RLock()
	pool, ok = r.pools[id]
	r.mu.RUnlock()
	if ok {
		return pool, nil
	}

	if cfg, err := r.fetchFromApigeeKVM(ctx, id); err == nil && cfg != nil {
		if _, err := r.Upsert(ctx, *cfg); err == nil {
			r.mu.RLock()
			pool = r.pools[id]
			r.mu.RUnlock()
			if pool != nil {
				log.Printf("Auto-hydrated datasource %q from Apigee KVM", id)
				return pool, nil
			}
		}
	}

	return nil, fmt.Errorf("datasource %q is not registered", id)
}

func (r *DatasourceRegistry) Delete(datasourceID string) bool {
	r.mu.Lock()
	pool, existed := r.pools[datasourceID]
	if existed {
		delete(r.pools, datasourceID)
	}
	r.mu.Unlock()
	if existed && pool != nil {
		pool.Close()
	}
	return existed
}

func (r *DatasourceRegistry) List() []DatasourceStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DatasourceStatus, 0, len(r.pools))
	for _, p := range r.pools {
		out = append(out, p.Status())
	}
	return out
}

func (r *DatasourceRegistry) CloseAll() {
	r.mu.Lock()
	pools := make([]*DatasourcePool, 0, len(r.pools))
	for k, p := range r.pools {
		pools = append(pools, p)
		delete(r.pools, k)
	}
	r.mu.Unlock()
	for _, p := range pools {
		p.Close()
	}
}

// SyncAllFromApigeeKVM loads all pre-established datasources from Apigee's Encrypted KVM on container startup.
func (r *DatasourceRegistry) SyncAllFromApigeeKVM(ctx context.Context) {
	if r.apigeeOrg == "" || r.apigeeEnv == "" || r.apigeeKVM == "" {
		return
	}
	token, err := r.getMetadataAccessToken(ctx)
	if err != nil || token == "" {
		return
	}
	apiURL := fmt.Sprintf("https://apigee.googleapis.com/v1/organizations/%s/environments/%s/keyvaluemaps/%s/entries",
		url.PathEscape(r.apigeeOrg), url.PathEscape(r.apigeeEnv), url.PathEscape(r.apigeeKVM))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}

	var payload struct {
		KeyValueEntries []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"keyValueEntries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return
	}

	for _, entry := range payload.KeyValueEntries {
		var cfg DatasourceConfig
		if err := json.Unmarshal([]byte(entry.Value), &cfg); err == nil {
			if cfg.DatasourceID == "" {
				cfg.DatasourceID = entry.Name
			}
			if _, err := r.Upsert(ctx, cfg); err != nil {
				log.Printf("WARN: Startup KVM hydration for %q failed: %v", cfg.DatasourceID, err)
			} else {
				log.Printf("Startup KVM hydration succeeded for datasource %q", cfg.DatasourceID)
			}
		}
	}
}

func (r *DatasourceRegistry) fetchFromApigeeKVM(ctx context.Context, datasourceID string) (*DatasourceConfig, error) {
	if r.apigeeOrg == "" || r.apigeeEnv == "" || r.apigeeKVM == "" {
		return nil, errors.New("Apigee KVM coordinates not configured")
	}
	token, err := r.getMetadataAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	apiURL := fmt.Sprintf("https://apigee.googleapis.com/v1/organizations/%s/environments/%s/keyvaluemaps/%s/entries/%s",
		url.PathEscape(r.apigeeOrg), url.PathEscape(r.apigeeEnv), url.PathEscape(r.apigeeKVM), url.PathEscape(datasourceID))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("KVM entry lookup returned %d", resp.StatusCode)
	}

	var entry struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		return nil, err
	}
	var cfg DatasourceConfig
	if err := json.Unmarshal([]byte(entry.Value), &cfg); err != nil {
		return nil, err
	}
	if cfg.DatasourceID == "" {
		cfg.DatasourceID = entry.Name
	}
	return &cfg, nil
}

func (r *DatasourceRegistry) getMetadataAccessToken(ctx context.Context) (string, error) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()
	if r.cachedToken != "" && time.Now().Before(r.tokenExpires) {
		return r.cachedToken, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("metadata server status %d: %s", resp.StatusCode, string(body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	ttl := time.Duration(tok.ExpiresIn-60) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	r.cachedToken = tok.AccessToken
	r.tokenExpires = time.Now().Add(ttl)
	return r.cachedToken, nil
}
