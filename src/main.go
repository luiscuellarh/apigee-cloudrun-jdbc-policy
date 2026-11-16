package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxRequestBodyBytes = 1 << 20 // 1 MB cap

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

type Server struct {
	registry *DatasourceRegistry
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.Writer.Write(b)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			_ = gz.Close()
			gzipWriterPool.Put(gz)
		}()
		next.ServeHTTP(&gzipResponseWriter{Writer: gz, ResponseWriter: w}, r)
	})
}

func writeError(w http.ResponseWriter, status int, datasourceID, errCode, message string) {
	logStructured("WARNING", "jdbc_policy_error", map[string]any{
		"http_status":   status,
		"datasource_id": datasourceID,
		"error_code":    errCode,
		"error_detail":  message,
	})
	writeJSON(w, status, map[string]any{
		"error":         "jdbc_policy_rejection",
		"datasource_id": datasourceID,
		"error_code":    errCode,
		"message":       message,
	})
}

// logStructured emits a single-line JSON log entry formatted for Google Cloud Logging.
func logStructured(severity, event string, fields map[string]any) {
	entry := make(map[string]any, len(fields)+3)
	entry["severity"] = severity
	entry["message"] = event
	entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	for k, v := range fields {
		entry[k] = v
	}
	if b, err := json.Marshal(entry); err == nil {
		os.Stdout.Write(append(b, '\n'))
	}
}

func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"datasources": s.registry.List(),
	})
}

// HandleDatasources implements Step 1 (Configure & Keep-Alive Datasource Pool):
//   - GET    /v1/jdbc/datasources[?id=...]
//   - POST   /v1/jdbc/datasources
//   - DELETE /v1/jdbc/datasources?id=...
func (s *Server) HandleDatasources(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	switch r.Method {
	case http.MethodGet:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id != "" {
			pool, err := s.registry.Resolve(r.Context(), id)
			if err != nil {
				writeError(w, http.StatusNotFound, id, "DATASOURCE_NOT_FOUND", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, pool.Status())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"datasources": s.registry.List(),
		})

	case http.MethodPost:
		var cfg DatasourceConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeError(w, http.StatusBadRequest, "", "INVALID_JSON", "Invalid JSON payload: "+err.Error())
			return
		}
		status, err := s.registry.Upsert(r.Context(), cfg)
		if err != nil {
			writeError(w, http.StatusBadRequest, cfg.DatasourceID, "DATASOURCE_CONNECTION_FAILED", err.Error())
			return
		}
		logStructured("INFO", "datasource_registered_and_warmed", map[string]any{
			"datasource_id":    status.DatasourceID,
			"driver":           status.Driver,
			"open_connections": status.OpenConnections,
			"ping_latency_ms":  status.LastPingLatencyMs,
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"success":            true,
			"status":             "registered_and_warmed",
			"datasource_id":      status.DatasourceID,
			"driver":             status.Driver,
			"masked_dsn":         status.MaskedDSN,
			"min_pool_size":      status.MinPoolSize,
			"max_pool_size":      status.MaxPoolSize,
			"keep_alive_sec":     status.KeepAliveSec,
			"open_connections":   status.OpenConnections,
			"idle_connections":   status.Idle,
			"ping_latency_ms":    status.LastPingLatencyMs,
			"last_keepalive_at":  status.LastKeepAliveAt,
		})

	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeError(w, http.StatusBadRequest, "", "MISSING_DATASOURCE_ID", "Query parameter 'id' is required for DELETE")
			return
		}
		existed := s.registry.Delete(id)
		if !existed {
			writeError(w, http.StatusNotFound, id, "DATASOURCE_NOT_FOUND", "Datasource not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":       true,
			"datasource_id": id,
			"status":        "deleted_and_pool_drained",
		})

	default:
		writeError(w, http.StatusMethodNotAllowed, "", "METHOD_NOT_ALLOWED", "Allowed methods: GET, POST, DELETE")
	}
}

// HandleQuery implements Step 2 (Consume Warm Pool from API Proxy via ServiceCallout):
//   - POST /v1/jdbc/query (also aliased at /v1/jdbc/execute and /v1/jdbc/procedure)
func (s *Server) HandleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "", "METHOD_NOT_ALLOWED", "Only POST is supported")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	var req JDBCQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "", "INVALID_JSON", "Invalid JSON payload: "+err.Error())
		return
	}

	req.DatasourceID = strings.TrimSpace(req.DatasourceID)
	if req.DatasourceID == "" {
		writeError(w, http.StatusBadRequest, "", "MISSING_DATASOURCE_ID", "datasource_id is required (configure it via jdbc.properties PropertySet)")
		return
	}

	pool, err := s.registry.Resolve(r.Context(), req.DatasourceID)
	if err != nil {
		writeError(w, http.StatusNotFound, req.DatasourceID, "DATASOURCE_NOT_FOUND", err.Error())
		return
	}

	resp, httpStatus, err := ExecuteJDBCPolicy(r.Context(), pool, req)
	if err != nil {
		errCode := "SQL_EXECUTION_ERROR"
		if httpStatus == http.StatusNotFound {
			errCode = "EMPTY_RESULT_SET"
		} else if httpStatus == http.StatusGatewayTimeout {
			errCode = "SQL_QUERY_TIMEOUT"
		} else if httpStatus == http.StatusBadRequest {
			errCode = "INVALID_SQL_REQUEST"
		}
		writeError(w, httpStatus, req.DatasourceID, errCode, err.Error())
		return
	}

	logStructured("INFO", "jdbc_query_executed", map[string]any{
		"datasource_id":     resp.DatasourceID,
		"driver":            resp.Driver,
		"operation_type":    resp.OperationType,
		"row_count":         resp.RowCount,
		"truncated":         resp.Truncated,
		"execution_time_ms": resp.ExecutionTimeMs,
		"http_status":       http.StatusOK,
	})
	writeJSON(w, http.StatusOK, resp)
}

func loadBootstrapDatasources(ctx context.Context, registry *DatasourceRegistry, configPath string) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("No bootstrap datasources file at %s: %v", configPath, err)
		return
	}
	var wrapper struct {
		Datasources []DatasourceConfig `json:"datasources"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		log.Printf("Failed parsing %s: %v", configPath, err)
		return
	}
	for _, cfg := range wrapper.Datasources {
		if _, err := registry.Upsert(ctx, cfg); err != nil {
			log.Printf("Failed initializing bootstrap datasource %q: %v", cfg.DatasourceID, err)
		} else {
			log.Printf("Initialized warm connection pool for datasource %q (%s)", cfg.DatasourceID, cfg.Driver)
		}
	}
}

func main() {
	ctx := context.Background()
	registry := NewDatasourceRegistry()

	configPath := os.Getenv("JDBC_DATASOURCES_CONFIG_PATH")
	if configPath == "" {
		configPath = "/etc/jdbc/datasources.json"
	}
	loadBootstrapDatasources(ctx, registry, configPath)

	// Automatically hydrate any pre-established datasources from Apigee's Encrypted KVM on startup
	registry.SyncAllFromApigeeKVM(ctx)

	srv := &Server{registry: registry}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.HandleHealthz)
	mux.HandleFunc("/v1/jdbc/datasources", srv.HandleDatasources)
	mux.HandleFunc("/v1/jdbc-admin/datasources", srv.HandleDatasources)
	mux.HandleFunc("/v1/jdbc/query", srv.HandleQuery)
	mux.HandleFunc("/v1/jdbc/execute", srv.HandleQuery)
	mux.HandleFunc("/v1/jdbc/procedure", srv.HandleQuery)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           withGzip(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("JDBC/ODBC Policy Connector listening on :%s", port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down gracefully and draining database connection pools...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutCtx)
	registry.CloseAll()
}
