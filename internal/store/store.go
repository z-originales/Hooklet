package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure go sqlite driver
)

// Webhook represents a registered webhook endpoint configuration
type Webhook struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// User represents a system user with access tokens
type User struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	TokenHash     string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	Subscriptions string    `json:"subscriptions"` // simple comma-separated list or "*"
}

type Store struct {
	db *sql.DB
}

func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	s := &Store{db: db}
	if err := s.init(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) init() error {
	schema := `
	CREATE TABLE IF NOT EXISTS webhooks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		token_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		subscriptions TEXT DEFAULT ''
	);
	`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("failed to init schema: %w", err)
	}
	return nil
}

// Webhook Methods

func (s *Store) CreateWebhook(name string) (*Webhook, error) {
	query := `INSERT INTO webhooks (name, created_at) VALUES (?, ?)`
	now := time.Now()
	res, err := s.db.Exec(query, name, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create webhook: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return &Webhook{
		ID:        id,
		Name:      name,
		CreatedAt: now,
	}, nil
}

func (s *Store) ListWebhooks() ([]Webhook, error) {
	query := `SELECT id, name, created_at FROM webhooks ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list webhooks: %w", err)
	}
	defer rows.Close()

	var webhooks []Webhook
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(&w.ID, &w.Name, &w.CreatedAt); err != nil {
			return nil, err
		}
		webhooks = append(webhooks, w)
	}
	return webhooks, nil
}

func (s *Store) DeleteWebhook(id int64) error {
	query := `DELETE FROM webhooks WHERE id = ?`
	_, err := s.db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete webhook: %w", err)
	}
	return nil
}

// User Methods

func (s *Store) CreateUser(name, tokenHash string, subscriptions string) (*User, error) {
	query := `INSERT INTO users (name, token_hash, subscriptions, created_at) VALUES (?, ?, ?, ?)`
	now := time.Now()
	res, err := s.db.Exec(query, name, tokenHash, subscriptions, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return &User{
		ID:            id,
		Name:          name,
		TokenHash:     tokenHash,
		Subscriptions: subscriptions,
		CreatedAt:     now,
	}, nil
}

func (s *Store) ListUsers() ([]User, error) {
	query := `SELECT id, name, created_at, subscriptions FROM users ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.CreatedAt, &u.Subscriptions); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

// GetUserByTokenHash is needed for validation later
func (s *Store) GetUserByTokenHash(hash string) (*User, error) {
	query := `SELECT id, name, created_at, subscriptions FROM users WHERE token_hash = ?`
	var u User
	err := s.db.QueryRow(query, hash).Scan(&u.ID, &u.Name, &u.CreatedAt, &u.Subscriptions)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	u.TokenHash = hash
	return &u, nil
}
