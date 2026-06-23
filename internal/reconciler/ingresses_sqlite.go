package reconciler

import (
	"database/sql"
	"fmt"

	"github.com/kkjorsvik/smith/internal/types"
	_ "github.com/mattn/go-sqlite3"
)

// IngressStore persists host -> service ingress mappings for host-based HTTPS
// routing.
type IngressStore struct {
	db *sql.DB
}

// NewIngressStore opens (or creates) the ingresses table in the SQLite
// database at path.
func NewIngressStore(path string) (*IngressStore, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	s := &IngressStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *IngressStore) migrate() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS ingresses (
			host    TEXT PRIMARY KEY,
			service TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create ingresses table: %w", err)
	}
	return nil
}

// Add creates or updates an ingress (keyed by host).
func (s *IngressStore) Add(ing types.Ingress) error {
	if ing.Host == "" {
		return fmt.Errorf("ingress host cannot be empty")
	}
	if ing.Service == "" {
		return fmt.Errorf("ingress %s: service cannot be empty", ing.Host)
	}
	if _, err := s.db.Exec(`
		INSERT INTO ingresses (host, service) VALUES (?, ?)
		ON CONFLICT(host) DO UPDATE SET service = excluded.service;
	`, ing.Host, ing.Service); err != nil {
		return fmt.Errorf("upsert ingress %s: %w", ing.Host, err)
	}
	return nil
}

// List returns all ingresses.
func (s *IngressStore) List() ([]types.Ingress, error) {
	rows, err := s.db.Query(`SELECT host, service FROM ingresses`)
	if err != nil {
		return nil, fmt.Errorf("query ingresses: %w", err)
	}
	defer rows.Close()

	var out []types.Ingress
	for rows.Next() {
		var ing types.Ingress
		if err := rows.Scan(&ing.Host, &ing.Service); err != nil {
			return nil, fmt.Errorf("scan ingress: %w", err)
		}
		out = append(out, ing)
	}
	return out, rows.Err()
}

// Remove deletes an ingress by host.
func (s *IngressStore) Remove(host string) error {
	if _, err := s.db.Exec(`DELETE FROM ingresses WHERE host = ?`, host); err != nil {
		return fmt.Errorf("remove ingress %s: %w", host, err)
	}
	return nil
}

// Close releases the database handle.
func (s *IngressStore) Close() error {
	return s.db.Close()
}
