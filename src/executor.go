package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// placeholderRegex matches both :param and ${param} (as well as $[param]) placeholders inside SQL templates.
var placeholderRegex = regexp.MustCompile(`\$\[[a-zA-Z0-9_.-]+\]|\$\{[a-zA-Z0-9_.-]+\}`)

// CompilePreparedSQL rewrites $[param] and ${param} placeholders into native
// database driver positional parameters ($1 for Postgres, @p1 for SQL Server,
// :1 for Oracle, ? for MySQL/SQLite) and returns the ordered argument slice.
func CompilePreparedSQL(sqlTemplate string, params map[string]any, sqlDriver string) (string, []any) {
	if !strings.Contains(sqlTemplate, "$") || len(params) == 0 {
		return sqlTemplate, nil
	}

	args := make([]any, 0, len(params))
	idx := 0
	compiled := placeholderRegex.ReplaceAllStringFunc(sqlTemplate, func(match string) string {
		if len(match) < 4 {
			return match
		}
		key := match[2 : len(match)-1]
		val, exists := params[key]
		if !exists {
			val = nil
		}
		idx++
		args = append(args, val)

		switch sqlDriver {
		case "pgx", "postgres":
			return fmt.Sprintf("$%d", idx)
		case "sqlserver":
			return fmt.Sprintf("@p%d", idx)
		case "oracle":
			return fmt.Sprintf(":%d", idx)
		default:
			return "?"
		}
	})
	return compiled, args
}

// IsRowReturningSQL determines whether a SQL statement returns a result set
// (SELECT, WITH, SHOW, EXPLAIN, PRAGMA, CALL, or DML with RETURNING).
func IsRowReturningSQL(sqlText string) bool {
	trimmed := strings.TrimSpace(sqlText)
	// Strip leading SQL comments
	for strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "/*") {
		if strings.HasPrefix(trimmed, "--") {
			if nl := strings.IndexByte(trimmed, '\n'); nl != -1 {
				trimmed = strings.TrimSpace(trimmed[nl+1:])
			} else {
				break
			}
		} else if strings.HasPrefix(trimmed, "/*") {
			if end := strings.Index(trimmed, "*/"); end != -1 {
				trimmed = strings.TrimSpace(trimmed[end+2:])
			} else {
				break
			}
		}
	}

	upper := strings.ToUpper(trimmed)
	for _, prefix := range []string{"SELECT", "WITH", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "PRAGMA", "CALL", "EXEC"} {
		if strings.HasPrefix(upper, prefix+" ") || strings.HasPrefix(upper, prefix+"\t") || strings.HasPrefix(upper, prefix+"\n") || upper == prefix {
			return true
		}
	}
	if strings.Contains(upper, " RETURNING ") || strings.HasSuffix(upper, " RETURNING *") {
		return true
	}
	return false
}

// JDBCQueryRequest represents the payload sent by Apigee's ServiceCallout policy.
type JDBCQueryRequest struct {
	DatasourceID      string         `json:"datasource_id"`
	SQL               string         `json:"sql"`
	Params            map[string]any `json:"params,omitempty"`
	VariablePrefix    string         `json:"variable_prefix,omitempty"`
	MaxRows           int            `json:"max_rows,omitempty"`
	FailIfEmpty       bool           `json:"fail_if_empty,omitempty"`
	ConvertToStrings  bool           `json:"convert_to_strings,omitempty"`
	UseTransaction    bool           `json:"use_transaction,omitempty"`
	TimeoutMs         int            `json:"timeout_ms,omitempty"`
}

// JDBCQueryResponse returns the structured result set and scoped context variables.
type JDBCQueryResponse struct {
	DatasourceID     string            `json:"datasource_id"`
	Driver           string            `json:"driver"`
	OperationType    string            `json:"operation_type"` // SELECT or DML
	Success          bool              `json:"success"`
	RowCount         int64             `json:"row_count"`
	Truncated        bool              `json:"truncated"`
	LastInsertID     int64             `json:"last_insert_id,omitempty"`
	Columns          []string          `json:"columns,omitempty"`
	FirstRow         map[string]any    `json:"first_row,omitempty"`
	Rows             []map[string]any  `json:"rows"`
	ContextVariables map[string]string `json:"context_variables"`
	ExecutionTimeMs  int64             `json:"execution_time_ms"`
}

func isStaleConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "bad connection") ||
		strings.Contains(msg, "closed network connection")
}

// ExecuteJDBCPolicy runs the SQL statement against the warm DatasourcePool with
// prepared statement parameter binding, automatic 1-attempt stale socket retry,
// max_rows cap, and variable_prefix context variable generation.
func ExecuteJDBCPolicy(ctx context.Context, pool *DatasourcePool, req JDBCQueryRequest) (*JDBCQueryResponse, int, error) {
	t0 := time.Now()
	sqlText := strings.TrimSpace(req.SQL)
	if sqlText == "" {
		return nil, 400, errors.New("sql statement is required")
	}

	prefix := strings.TrimSpace(req.VariablePrefix)
	if prefix == "" {
		prefix = "jdbc"
	}
	maxRows := req.MaxRows
	if maxRows <= 0 {
		maxRows = 100
	}
	if maxRows > 5000 {
		maxRows = 5000
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = pool.Config.QueryTimeoutMs
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}

	qCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	compiledSQL, args := CompilePreparedSQL(sqlText, req.Params, pool.SQLDriverName)

	if IsRowReturningSQL(compiledSQL) {
		resp, err := executeSelectWithRetry(qCtx, pool, compiledSQL, args, prefix, maxRows, req.ConvertToStrings)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, 504, fmt.Errorf("SQL query timed out after %d ms: %w", timeoutMs, err)
			}
			return nil, 422, err
		}
		resp.ExecutionTimeMs = time.Since(t0).Milliseconds()
		if req.FailIfEmpty && resp.RowCount == 0 {
			return resp, 404, errors.New("query returned 0 rows and fail_if_empty is true")
		}
		return resp, 200, nil
	}

	resp, err := executeDMLWithRetry(qCtx, pool, compiledSQL, args, prefix, req.UseTransaction)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, 504, fmt.Errorf("SQL execution timed out after %d ms: %w", timeoutMs, err)
		}
		return nil, 422, err
	}
	resp.ExecutionTimeMs = time.Since(t0).Milliseconds()
	return resp, 200, nil
}

func executeSelectWithRetry(ctx context.Context, pool *DatasourcePool, compiledSQL string, args []any, prefix string, maxRows int, convertToStrings bool) (*JDBCQueryResponse, error) {
	var rows *sql.Rows
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if stmt, prepErr := pool.GetOrPrepareStmt(ctx, compiledSQL); prepErr == nil {
			rows, err = stmt.QueryContext(ctx, args...)
			if err != nil {
				pool.EvictStmt(compiledSQL)
			}
		} else {
			rows, err = pool.DB.QueryContext(ctx, compiledSQL, args...)
		}
		if err == nil || !isStaleConnError(err) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	resultRows := make([]map[string]any, 0, 8)
	var firstRow map[string]any
	truncated := false

	// Allocate scan buffers once outside the row loop to reduce GC pressure on multi-row SELECTs
	rawVals := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range rawVals {
		dest[i] = &rawVals[i]
	}

	for rows.Next() {
		if len(resultRows) >= maxRows {
			truncated = true
			break
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		rowMap := make(map[string]any, len(cols))
		for i, colName := range cols {
			val := normalizeSQLValue(rawVals[i], convertToStrings)
			rowMap[colName] = val
		}
		if firstRow == nil {
			firstRow = rowMap
		}
		resultRows = append(resultRows, rowMap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	count := int64(len(resultRows))
	ctxVars := map[string]string{
		fmt.Sprintf("%s.row_count", prefix): fmt.Sprintf("%d", count),
		fmt.Sprintf("%s.truncated", prefix): fmt.Sprintf("%t", truncated),
	}
	for k, v := range firstRow {
		if v == nil {
			ctxVars[fmt.Sprintf("%s.%s", prefix, k)] = ""
		} else {
			ctxVars[fmt.Sprintf("%s.%s", prefix, k)] = fmt.Sprintf("%v", v)
		}
	}

	if firstRow == nil {
		firstRow = map[string]any{}
	}

	return &JDBCQueryResponse{
		DatasourceID:     pool.Config.DatasourceID,
		Driver:           pool.Config.Driver,
		OperationType:    "SELECT",
		Success:          true,
		RowCount:         count,
		Truncated:        truncated,
		Columns:          cols,
		FirstRow:         firstRow,
		Rows:             resultRows,
		ContextVariables: ctxVars,
	}, nil
}

func executeDMLWithRetry(ctx context.Context, pool *DatasourcePool, compiledSQL string, args []any, prefix string, useTx bool) (*JDBCQueryResponse, error) {
	var res sql.Result
	var err error

	for attempt := 0; attempt < 2; attempt++ {
		if useTx {
			var tx *sql.Tx
			tx, err = pool.DB.BeginTx(ctx, nil)
			if err == nil {
				res, err = tx.ExecContext(ctx, compiledSQL, args...)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
		} else {
			if stmt, prepErr := pool.GetOrPrepareStmt(ctx, compiledSQL); prepErr == nil {
				res, err = stmt.ExecContext(ctx, args...)
				if err != nil {
					pool.EvictStmt(compiledSQL)
				}
			} else {
				res, err = pool.DB.ExecContext(ctx, compiledSQL, args...)
			}
		}
		if err == nil || !isStaleConnError(err) {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	var rowsAffected int64
	var lastInsertID int64
	if res != nil {
		rowsAffected, _ = res.RowsAffected()
		lastInsertID, _ = res.LastInsertId()
	}

	ctxVars := map[string]string{
		fmt.Sprintf("%s.row_count", prefix): fmt.Sprintf("%d", rowsAffected),
		fmt.Sprintf("%s.truncated", prefix): "false",
	}
	if lastInsertID > 0 {
		ctxVars[fmt.Sprintf("%s.last_insert_id", prefix)] = fmt.Sprintf("%d", lastInsertID)
	}

	return &JDBCQueryResponse{
		DatasourceID:     pool.Config.DatasourceID,
		Driver:           pool.Config.Driver,
		OperationType:    "DML",
		Success:          true,
		RowCount:         rowsAffected,
		Truncated:        false,
		LastInsertID:     lastInsertID,
		FirstRow:         map[string]any{},
		Rows:             []map[string]any{},
		ContextVariables: ctxVars,
	}, nil
}

func normalizeSQLValue(v any, forceString bool) any {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case []byte:
		s := string(val)
		return s
	case time.Time:
		return val.UTC().Format(time.RFC3339)
	default:
		if forceString {
			return fmt.Sprintf("%v", val)
		}
		return val
	}
}
