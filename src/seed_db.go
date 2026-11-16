package main

import (
	"context"
	"database/sql"
	"time"
)

// SeedEnterpriseSQLite initializes the shared in-memory SQLite database with
// realistic enterprise tables (subscribers, orders, audit_log) so the built-in
// "enterprise-sqlite" datasource works immediately with zero external dependencies.
func SeedEnterpriseSQLite(ctx context.Context, db *sql.DB) error {
	schemaSQL := []string{
		`CREATE TABLE IF NOT EXISTS subscribers (
			id INTEGER PRIMARY KEY,
			external_id TEXT NOT NULL UNIQUE,
			full_name TEXT NOT NULL,
			email TEXT NOT NULL,
			segment TEXT NOT NULL,
			plan_tier TEXT NOT NULL,
			status TEXT NOT NULL,
			credit_limit REAL NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS orders (
			order_id INTEGER PRIMARY KEY AUTOINCREMENT,
			subscriber_id INTEGER NOT NULL,
			sku TEXT NOT NULL,
			amount REAL NOT NULL,
			currency TEXT NOT NULL,
			order_status TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (subscriber_id) REFERENCES subscribers(id)
		);`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			log_id INTEGER PRIMARY KEY AUTOINCREMENT,
			actor TEXT NOT NULL,
			action TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
	}

	for _, stmt := range schemaSQL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	seedStmts := []string{
		`INSERT OR IGNORE INTO subscribers (id, external_id, full_name, email, segment, plan_tier, status, credit_limit, created_at) VALUES
			(1001, 'SUB-1001', 'Elena Rostova', 'elena.rostova@enterprise.example.com', 'ENTERPRISE', 'PLATINUM_5G', 'ACTIVE', 25000.00, '` + now + `'),
			(1002, 'SUB-1002', 'Carlos Mendoza', 'carlos.mendoza@enterprise.example.com', 'CORPORATE', 'GOLD_FIBER', 'ACTIVE', 12000.00, '` + now + `'),
			(1003, 'SUB-1003', 'Hannah Abbott', 'hannah.abbott@enterprise.example.com', 'SME', 'SILVER_IOT', 'SUSPENDED', 3500.00, '` + now + `');`,
		`INSERT OR IGNORE INTO orders (order_id, subscriber_id, sku, amount, currency, order_status, updated_at) VALUES
			(5001, 1001, 'DEDICATED-MPLS-10G', 4200.50, 'EUR', 'PROVISIONED', '` + now + `'),
			(5002, 1001, 'EDGE-SDWAN-PACK', 890.00, 'EUR', 'ACTIVE', '` + now + `'),
			(5003, 1002, 'SIP-TRUNK-500', 650.00, 'EUR', 'PROVISIONED', '` + now + `');`,
	}

	for _, stmt := range seedStmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
