package reconciler

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kkjorsvik/smith/internal/types"
	_ "github.com/mattn/go-sqlite3"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	store := &SQLiteStore{db: db}
	if err := store.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return store, nil
}

// migrate brings the schema up to date. It is idempotent and safe to run
// on every startup, against both a fresh database and one created by an
// older version of smith.
func (s *SQLiteStore) migrate() error {
	// Base table for fresh installs. CREATE TABLE IF NOT EXISTS never
	// alters an existing table, so column additions are handled below.
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS workloads (
			id    TEXT PRIMARY KEY,
			image TEXT NOT NULL,
			args  TEXT NOT NULL,
			health_check TEXT
		);
	`); err != nil {
		return fmt.Errorf("create workloads table: %w", err)
	}

	// Incremental column migrations for databases created before a column
	// existed. Each entry is applied only if the column is missing, so this
	// stays idempotent and avoids "duplicate column name" errors.
	columns := []struct {
		name string
		ddl  string
	}{
		{"health_check", "ALTER TABLE workloads ADD COLUMN health_check TEXT"},
	}

	for _, c := range columns {
		exists, err := s.columnExists("workloads", c.name)
		if err != nil {
			return fmt.Errorf("check column %s: %w", c.name, err)
		}
		if exists {
			continue
		}
		if _, err := s.db.Exec(c.ddl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}

	return nil
}

// columnExists reports whether table has a column with the given name.
func (s *SQLiteStore) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("table_info(%s): %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			dfltValue  sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &primaryKey); err != nil {
			return false, fmt.Errorf("scan table_info: %w", err)
		}
		if name == column {
			return true, nil
		}
	}

	return false, rows.Err()
}

func (s *SQLiteStore) Add(w types.Workload) error {
	if w.ID == "" {
		return fmt.Errorf("workload ID cannot be empty")
	}
	if w.Image == "" {
		return fmt.Errorf("workload %s: image cannot be empty", w.ID)
	}

	args, err := json.Marshal(w.Args)
	if err != nil {
		return fmt.Errorf("marshal args for %s: %w", w.ID, err)
	}

	var hc []byte
	if w.HealthCheck != nil {
		hc, err = json.Marshal(w.HealthCheck)
		if err != nil {
			return fmt.Errorf("marshal health check for %s: %w", w.ID, err)
		}
	}

	_, err = s.db.Exec(`
		INSERT INTO workloads (id, image, args, health_check)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			image        = excluded.image,
			args         = excluded.args,
			health_check = excluded.health_check;
	`, w.ID, w.Image, string(args), string(hc))
	if err != nil {
		return fmt.Errorf("upsert workload %s: %w", w.ID, err)
	}

	return nil
}

func (s *SQLiteStore) Remove(id string) error {
	_, err := s.db.Exec(`DELETE FROM workloads WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove workload %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) List() (map[string]types.Workload, error) {
	// COALESCE guards against NULL health_check values, which exist on rows
	// created before the column was added by an ALTER TABLE migration.
	rows, err := s.db.Query(`SELECT id, image, args, COALESCE(health_check, '') FROM workloads`)
	if err != nil {
		return nil, fmt.Errorf("query workloads: %w", err)
	}
	defer rows.Close()

	out := make(map[string]types.Workload)
	for rows.Next() {
		var w types.Workload
		var args string
		var hc string

		if err := rows.Scan(&w.ID, &w.Image, &args, &hc); err != nil {
			return nil, fmt.Errorf("scan workload: %w", err)
		}

		if err := json.Unmarshal([]byte(args), &w.Args); err != nil {
			return nil, fmt.Errorf("unmarshal args for %s: %w", w.ID, err)
		}

		if hc != "" && hc != "null" {
			w.HealthCheck = &types.HealthCheck{}
			if err := json.Unmarshal([]byte(hc), w.HealthCheck); err != nil {
				return nil, fmt.Errorf("unmarshal health check for %s: %w", w.ID, err)
			}
		}

		out[w.ID] = w
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return out, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
