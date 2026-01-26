package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure go sqlite driver
)

// Webhook represents a registered webhook endpoint configuration
type Webhook struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	TopicHash string    `json:"topic_hash"` // SHA256 of Name
	CreatedAt time.Time `json:"created_at"`
}

// Consumer represents a WebSocket client that consumes webhooks.
// Consumers authenticate with tokens and can subscribe to specific topics.
type Consumer struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	TokenHash     string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	Subscriptions string    `json:"subscriptions"` // comma-separated topic names or "*" for all
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
	// We add topic_hash to webhooks.
	// Note: If updating from an old DB, we might need a migration.
	// For simplicity in this dev phase, we try to add the column if it's missing (via schema evolution)
	// or just rely on fresh DB.
	// Here we define the DESIRED schema.

	queries := []string{
		`CREATE TABLE IF NOT EXISTS webhooks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			topic_hash TEXT NOT NULL DEFAULT '', 
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,
		// Ensure topic_hash column exists for existing tables (migration)
		// SQLite ignores ADD COLUMN if it exists only in newer versions, but standard way is try/catch logic.
		// A simple hack for dev: "ALTER TABLE webhooks ADD COLUMN topic_hash TEXT NOT NULL DEFAULT ''"
		// We'll run it and ignore "duplicate column" error implicitly or just handle it if we want to be clean.
		// But modernc/sqlite might return error.
		// Let's just create indexes which is safer.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_webhooks_topic_hash ON webhooks(topic_hash);`,

		`CREATE TABLE IF NOT EXISTS consumers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			token_hash TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			subscriptions TEXT DEFAULT ''
		);`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("failed to init schema (query: %s): %w", q, err)
		}
	}

	// Migration attempt: Try to add column if it was missing from original creation
	// This will fail if column exists, so we ignore the error.
	s.db.Exec(`ALTER TABLE webhooks ADD COLUMN topic_hash TEXT NOT NULL DEFAULT ''`)

	return nil
}

// Helper: HashString returns SHA256 hex string
func HashString(input string) string {
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])
}

// Webhook Methods

func (s *Store) CreateWebhook(name string) (*Webhook, error) {
	hash := HashString(name)

	query := `INSERT INTO webhooks (name, topic_hash, created_at) VALUES (?, ?, ?)`
	now := time.Now()
	res, err := s.db.Exec(query, name, hash, now)
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
		TopicHash: hash,
		CreatedAt: now,
	}, nil
}

// GetWebhookByName retrieves a webhook by name (for admin).
func (s *Store) GetWebhookByName(name string) (*Webhook, error) {
	query := `SELECT id, name, topic_hash, created_at FROM webhooks WHERE name = ?`
	var w Webhook
	err := s.db.QueryRow(query, name).Scan(&w.ID, &w.Name, &w.TopicHash, &w.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

// GetWebhookByHash retrieves a webhook by its topic hash (for ingestion).
func (s *Store) GetWebhookByHash(hash string) (*Webhook, error) {
	query := `SELECT id, name, topic_hash, created_at FROM webhooks WHERE topic_hash = ?`
	var w Webhook
	err := s.db.QueryRow(query, hash).Scan(&w.ID, &w.Name, &w.TopicHash, &w.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &w, nil
}

func (s *Store) ListWebhooks() ([]Webhook, error) {
	query := `SELECT id, name, topic_hash, created_at FROM webhooks ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list webhooks: %w", err)
	}
	defer rows.Close()

	var webhooks []Webhook
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(&w.ID, &w.Name, &w.TopicHash, &w.CreatedAt); err != nil {
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

// Consumer Methods

func (s *Store) CreateConsumer(name, token string, subscriptions string) (*Consumer, error) {
	// IMPORTANT: Hash token before storing
	tokenHash := HashString(token)

	query := `INSERT INTO consumers (name, token_hash, subscriptions, created_at) VALUES (?, ?, ?, ?)`
	now := time.Now()
	res, err := s.db.Exec(query, name, tokenHash, subscriptions, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create consumer: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return &Consumer{
		ID:            id,
		Name:          name,
		TokenHash:     tokenHash, // Stored hash
		Subscriptions: subscriptions,
		CreatedAt:     now,
	}, nil
}

// GetConsumerByToken validates a token by hashing it and checking the DB.
func (s *Store) GetConsumerByToken(token string) (*Consumer, error) {
	hash := HashString(token)
	return s.GetConsumerByTokenHash(hash)
}

func (s *Store) ListConsumers() ([]Consumer, error) {
	query := `SELECT id, name, created_at, subscriptions FROM consumers ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list consumers: %w", err)
	}
	defer rows.Close()

	var consumers []Consumer
	for rows.Next() {
		var c Consumer
		if err := rows.Scan(&c.ID, &c.Name, &c.CreatedAt, &c.Subscriptions); err != nil {
			return nil, err
		}
		consumers = append(consumers, c)
	}
	return consumers, nil
}

// GetConsumerByTokenHash retrieves a consumer by their hashed token.
func (s *Store) GetConsumerByTokenHash(hash string) (*Consumer, error) {
	query := `SELECT id, name, created_at, subscriptions FROM consumers WHERE token_hash = ?`
	var c Consumer
	err := s.db.QueryRow(query, hash).Scan(&c.ID, &c.Name, &c.CreatedAt, &c.Subscriptions)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	c.TokenHash = hash
	return &c, nil
}

// DeleteConsumer removes a consumer by ID.
func (s *Store) DeleteConsumer(id int64) error {
	query := `DELETE FROM consumers WHERE id = ?`
	res, err := s.db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete consumer: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("consumer not found")
	}
	return nil
}

// UpdateConsumerSubscriptions updates a consumer's subscriptions.
func (s *Store) UpdateConsumerSubscriptions(id int64, subscriptions string) error {
	query := `UPDATE consumers SET subscriptions = ? WHERE id = ?`
	res, err := s.db.Exec(query, subscriptions, id)
	if err != nil {
		return fmt.Errorf("failed to update consumer: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("consumer not found")
	}
	return nil
}

// RegenerateConsumerToken updates a consumer's token hash.
func (s *Store) RegenerateConsumerToken(id int64, newTokenHash string) error {
	query := `UPDATE consumers SET token_hash = ? WHERE id = ?`
	res, err := s.db.Exec(query, newTokenHash, id)
	if err != nil {
		return fmt.Errorf("failed to regenerate token: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("consumer not found")
	}
	return nil
}
