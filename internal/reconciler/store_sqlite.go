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

func (s *SQLiteStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS workloads (
			id    TEXT PRIMARY KEY,
			image TEXT NOT NULL,
			args  TEXT NOT NULL,
			health_check TEXT
		);
	`)
	return err
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
	rows, err := s.db.Query(`SELECT id, image, args, health_check FROM workloads`)
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
