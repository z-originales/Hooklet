package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure go sqlite driver
)

// Topic represents a subscription target. Can be an exact topic name or a pattern.
// Patterns use glob syntax: "*" matches one level, "**" matches all levels.
type Topic struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`       // e.g., "orders.created" or "orders.*"
	IsPattern bool      `json:"is_pattern"` // true if Name contains wildcards
	CreatedAt time.Time `json:"created_at"`
}

// Webhook represents a registered webhook endpoint configuration.
// Each webhook is linked to exactly one exact topic.
type Webhook struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	TopicID   int64     `json:"topic_id"`   // FK to topics (exact topic only)
	TopicHash string    `json:"topic_hash"` // SHA256 of Name for URL
	TokenHash *string   `json:"-"`          // Optional: SHA256 of producer auth token (never exposed)
	HasToken  bool      `json:"has_token"`  // Indicates if authentication is required
	CreatedAt time.Time `json:"created_at"`
}

// Consumer represents a WebSocket client that consumes webhooks.
// Consumers authenticate with tokens and subscribe to topics via topic_subscriptions.
type Consumer struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// TopicSubscription represents a consumer's subscription to a topic.
type TopicSubscription struct {
	ID         int64     `json:"id"`
	ConsumerID int64     `json:"consumer_id"`
	TopicID    int64     `json:"topic_id"`
	CreatedAt  time.Time `json:"created_at"`
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

	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
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
	queries := []string{
		// Topics table: central entity for subscriptions
		`CREATE TABLE IF NOT EXISTS topics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			is_pattern BOOLEAN NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,

		// Webhooks table: each webhook links to exactly one exact topic
		`CREATE TABLE IF NOT EXISTS webhooks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			topic_id INTEGER NOT NULL,
			topic_hash TEXT NOT NULL,
			token_hash TEXT DEFAULT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (topic_id) REFERENCES topics(id) ON DELETE RESTRICT
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_webhooks_topic_hash ON webhooks(topic_hash);`,

		// Consumers table: WebSocket clients
		`CREATE TABLE IF NOT EXISTS consumers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			token_hash TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,

		// Topic subscriptions: N-N relationship between consumers and topics
		`CREATE TABLE IF NOT EXISTS topic_subscriptions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			consumer_id INTEGER NOT NULL,
			topic_id INTEGER NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (consumer_id) REFERENCES consumers(id) ON DELETE CASCADE,
			FOREIGN KEY (topic_id) REFERENCES topics(id) ON DELETE CASCADE,
			UNIQUE(consumer_id, topic_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_topic_subscriptions_consumer ON topic_subscriptions(consumer_id);`,
		`CREATE INDEX IF NOT EXISTS idx_topic_subscriptions_topic ON topic_subscriptions(topic_id);`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("failed to init schema (query: %s): %w", q, err)
		}
	}

	return nil
}

// HashString returns SHA256 hex string
func HashString(input string) string {
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])
}

// isPattern checks if a topic name contains wildcard characters
func isPattern(name string) bool {
	return strings.Contains(name, "*")
}

// Topic Methods

// CreateTopic creates a new topic (exact or pattern).
func (s *Store) CreateTopic(name string) (*Topic, error) {
	pattern := isPattern(name)

	query := `INSERT INTO topics (name, is_pattern, created_at) VALUES (?, ?, ?)`
	now := time.Now()
	res, err := s.db.Exec(query, name, pattern, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create topic: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return &Topic{
		ID:        id,
		Name:      name,
		IsPattern: pattern,
		CreatedAt: now,
	}, nil
}

// GetTopicByName retrieves a topic by name.
func (s *Store) GetTopicByName(name string) (*Topic, error) {
	query := `SELECT id, name, is_pattern, created_at FROM topics WHERE name = ?`
	var t Topic
	err := s.db.QueryRow(query, name).Scan(&t.ID, &t.Name, &t.IsPattern, &t.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// GetOrCreateTopic gets a topic by name, creating it if it doesn't exist.
func (s *Store) GetOrCreateTopic(name string) (*Topic, error) {
	topic, err := s.GetTopicByName(name)
	if err != nil {
		return nil, err
	}
	if topic != nil {
		return topic, nil
	}
	return s.CreateTopic(name)
}

// ListTopics returns all topics.
func (s *Store) ListTopics() ([]Topic, error) {
	query := `SELECT id, name, is_pattern, created_at FROM topics ORDER BY name`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list topics: %w", err)
	}
	defer rows.Close()

	var topics []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.Name, &t.IsPattern, &t.CreatedAt); err != nil {
			return nil, err
		}
		topics = append(topics, t)
	}
	return topics, nil
}

// DeleteTopic removes a topic by ID.
// Will fail if any webhooks reference this topic (RESTRICT).
func (s *Store) DeleteTopic(id int64) error {
	query := `DELETE FROM topics WHERE id = ?`
	res, err := s.db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete topic: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("topic not found")
	}
	return nil
}

// Webhook Methods

// CreateWebhook creates a webhook and its associated exact topic (without authentication token).
func (s *Store) CreateWebhook(name string) (*Webhook, error) {
	return s.CreateWebhookWithToken(name, "")
}

// CreateWebhookWithToken creates a webhook with an optional authentication token.
// If token is empty, the webhook will not require authentication.
// The token is hashed before storage and cannot be retrieved later.
func (s *Store) CreateWebhookWithToken(name, token string) (*Webhook, error) {
	// Webhooks cannot be patterns
	if isPattern(name) {
		return nil, fmt.Errorf("webhook name cannot contain wildcards")
	}

	// Get or create the exact topic for this webhook
	topic, err := s.GetOrCreateTopic(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get/create topic: %w", err)
	}

	hash := HashString(name)
	now := time.Now()

	var tokenHash *string
	if token != "" {
		h := HashString(token)
		tokenHash = &h
	}

	query := `INSERT INTO webhooks (name, topic_id, topic_hash, token_hash, created_at) VALUES (?, ?, ?, ?, ?)`
	res, err := s.db.Exec(query, name, topic.ID, hash, tokenHash, now)
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
		TopicID:   topic.ID,
		TopicHash: hash,
		TokenHash: tokenHash,
		HasToken:  tokenHash != nil,
		CreatedAt: now,
	}, nil
}

// GetWebhookByName retrieves a webhook by name.
func (s *Store) GetWebhookByName(name string) (*Webhook, error) {
	query := `SELECT id, name, topic_id, topic_hash, token_hash, created_at FROM webhooks WHERE name = ?`
	var w Webhook
	var tokenHash sql.NullString
	err := s.db.QueryRow(query, name).Scan(&w.ID, &w.Name, &w.TopicID, &w.TopicHash, &tokenHash, &w.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if tokenHash.Valid {
		w.TokenHash = &tokenHash.String
		w.HasToken = true
	}
	return &w, nil
}

// GetWebhookByHash retrieves a webhook by its topic hash (for ingestion).
func (s *Store) GetWebhookByHash(hash string) (*Webhook, error) {
	query := `SELECT id, name, topic_id, topic_hash, token_hash, created_at FROM webhooks WHERE topic_hash = ?`
	var w Webhook
	var tokenHash sql.NullString
	err := s.db.QueryRow(query, hash).Scan(&w.ID, &w.Name, &w.TopicID, &w.TopicHash, &tokenHash, &w.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if tokenHash.Valid {
		w.TokenHash = &tokenHash.String
		w.HasToken = true
	}
	return &w, nil
}

// ListWebhooks returns all webhooks.
func (s *Store) ListWebhooks() ([]Webhook, error) {
	query := `SELECT id, name, topic_id, topic_hash, token_hash, created_at FROM webhooks ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list webhooks: %w", err)
	}
	defer rows.Close()

	var webhooks []Webhook
	for rows.Next() {
		var w Webhook
		var tokenHash sql.NullString
		if err := rows.Scan(&w.ID, &w.Name, &w.TopicID, &w.TopicHash, &tokenHash, &w.CreatedAt); err != nil {
			return nil, err
		}
		if tokenHash.Valid {
			w.TokenHash = &tokenHash.String
			w.HasToken = true
		}
		webhooks = append(webhooks, w)
	}
	return webhooks, nil
}

// DeleteWebhook removes a webhook by ID.
// Note: The associated topic is NOT deleted (other consumers may reference it).
func (s *Store) DeleteWebhook(id int64) error {
	query := `DELETE FROM webhooks WHERE id = ?`
	_, err := s.db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to delete webhook: %w", err)
	}
	return nil
}

// SetWebhookToken sets or updates the authentication token for a webhook.
// The token is hashed before storage and cannot be retrieved later.
func (s *Store) SetWebhookToken(id int64, tokenHash string) error {
	query := `UPDATE webhooks SET token_hash = ? WHERE id = ?`
	res, err := s.db.Exec(query, tokenHash, id)
	if err != nil {
		return fmt.Errorf("failed to set webhook token: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("webhook not found")
	}
	return nil
}

// ClearWebhookToken removes the authentication token from a webhook.
// After this, the webhook will no longer require authentication.
func (s *Store) ClearWebhookToken(id int64) error {
	query := `UPDATE webhooks SET token_hash = NULL WHERE id = ?`
	res, err := s.db.Exec(query, id)
	if err != nil {
		return fmt.Errorf("failed to clear webhook token: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("webhook not found")
	}
	return nil
}

// GetWebhookByID retrieves a webhook by its ID.
func (s *Store) GetWebhookByID(id int64) (*Webhook, error) {
	query := `SELECT id, name, topic_id, topic_hash, token_hash, created_at FROM webhooks WHERE id = ?`
	var w Webhook
	var tokenHash sql.NullString
	err := s.db.QueryRow(query, id).Scan(&w.ID, &w.Name, &w.TopicID, &w.TopicHash, &tokenHash, &w.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if tokenHash.Valid {
		w.TokenHash = &tokenHash.String
		w.HasToken = true
	}
	return &w, nil
}

// Consumer Methods

// CreateConsumer creates a new consumer.
func (s *Store) CreateConsumer(name, token string) (*Consumer, error) {
	tokenHash := HashString(token)

	query := `INSERT INTO consumers (name, token_hash, created_at) VALUES (?, ?, ?)`
	now := time.Now()
	res, err := s.db.Exec(query, name, tokenHash, now)
	if err != nil {
		return nil, fmt.Errorf("failed to create consumer: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return &Consumer{
		ID:        id,
		Name:      name,
		TokenHash: tokenHash,
		CreatedAt: now,
	}, nil
}

// GetConsumerByToken validates a token by hashing it and checking the DB.
func (s *Store) GetConsumerByToken(token string) (*Consumer, error) {
	hash := HashString(token)
	return s.GetConsumerByTokenHash(hash)
}

// GetConsumerByTokenHash retrieves a consumer by their hashed token.
func (s *Store) GetConsumerByTokenHash(hash string) (*Consumer, error) {
	query := `SELECT id, name, token_hash, created_at FROM consumers WHERE token_hash = ?`
	var c Consumer
	err := s.db.QueryRow(query, hash).Scan(&c.ID, &c.Name, &c.TokenHash, &c.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// GetConsumerByID retrieves a consumer by ID.
func (s *Store) GetConsumerByID(id int64) (*Consumer, error) {
	query := `SELECT id, name, token_hash, created_at FROM consumers WHERE id = ?`
	var c Consumer
	err := s.db.QueryRow(query, id).Scan(&c.ID, &c.Name, &c.TokenHash, &c.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// ListConsumers returns all consumers.
func (s *Store) ListConsumers() ([]Consumer, error) {
	query := `SELECT id, name, created_at FROM consumers ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list consumers: %w", err)
	}
	defer rows.Close()

	var consumers []Consumer
	for rows.Next() {
		var c Consumer
		if err := rows.Scan(&c.ID, &c.Name, &c.CreatedAt); err != nil {
			return nil, err
		}
		consumers = append(consumers, c)
	}
	return consumers, nil
}

// DeleteConsumer removes a consumer by ID.
// Associated subscriptions are automatically deleted via CASCADE.
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

// Subscription Methods

// Subscribe adds a subscription for a consumer to a topic.
// If the topic doesn't exist, it will be created (allows pattern subscriptions).
func (s *Store) Subscribe(consumerID int64, topicName string) error {
	// Get or create the topic
	topic, err := s.GetOrCreateTopic(topicName)
	if err != nil {
		return fmt.Errorf("failed to get/create topic: %w", err)
	}

	query := `INSERT OR IGNORE INTO topic_subscriptions (consumer_id, topic_id, created_at) VALUES (?, ?, ?)`
	_, err = s.db.Exec(query, consumerID, topic.ID, time.Now())
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}
	return nil
}

// Unsubscribe removes a subscription for a consumer from a topic.
func (s *Store) Unsubscribe(consumerID int64, topicName string) error {
	topic, err := s.GetTopicByName(topicName)
	if err != nil {
		return err
	}
	if topic == nil {
		return fmt.Errorf("topic not found")
	}

	query := `DELETE FROM topic_subscriptions WHERE consumer_id = ? AND topic_id = ?`
	res, err := s.db.Exec(query, consumerID, topic.ID)
	if err != nil {
		return fmt.Errorf("failed to unsubscribe: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("subscription not found")
	}
	return nil
}

// GetConsumerSubscriptions returns all topics a consumer is subscribed to.
func (s *Store) GetConsumerSubscriptions(consumerID int64) ([]Topic, error) {
	query := `
		SELECT t.id, t.name, t.is_pattern, t.created_at 
		FROM topics t
		JOIN topic_subscriptions ts ON t.id = ts.topic_id
		WHERE ts.consumer_id = ?
		ORDER BY t.name
	`
	rows, err := s.db.Query(query, consumerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get subscriptions: %w", err)
	}
	defer rows.Close()

	var topics []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.Name, &t.IsPattern, &t.CreatedAt); err != nil {
			return nil, err
		}
		topics = append(topics, t)
	}
	return topics, nil
}

// SetConsumerSubscriptions replaces all subscriptions for a consumer.
// topicNames is a comma-separated list of topic names (can include patterns like "orders.*" or "**").
func (s *Store) SetConsumerSubscriptions(consumerID int64, topicNames string) error {
	// Start transaction
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Clear existing subscriptions
	if _, err := tx.Exec(`DELETE FROM topic_subscriptions WHERE consumer_id = ?`, consumerID); err != nil {
		return fmt.Errorf("failed to clear subscriptions: %w", err)
	}

	// Add new subscriptions
	if topicNames != "" {
		for _, name := range strings.Split(topicNames, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}

			// Get or create topic
			topic, err := s.GetTopicByName(name)
			if err != nil {
				return err
			}
			if topic == nil {
				// Create the topic
				pattern := isPattern(name)
				res, err := tx.Exec(`INSERT INTO topics (name, is_pattern, created_at) VALUES (?, ?, ?)`,
					name, pattern, time.Now())
				if err != nil {
					return fmt.Errorf("failed to create topic %s: %w", name, err)
				}
				topicID, _ := res.LastInsertId()
				topic = &Topic{ID: topicID, Name: name, IsPattern: pattern}
			}

			// Create subscription
			if _, err := tx.Exec(`INSERT OR IGNORE INTO topic_subscriptions (consumer_id, topic_id, created_at) VALUES (?, ?, ?)`,
				consumerID, topic.ID, time.Now()); err != nil {
				return fmt.Errorf("failed to add subscription: %w", err)
			}
		}
	}

	return tx.Commit()
}

// Pattern Matching

// MatchTopic checks if a webhook topic name matches a subscription pattern.
// Patterns:
//   - "**" matches everything (subscribe to all)
//   - "orders.*" matches "orders.created" but not "orders.eu.created"
//   - "orders.**" matches "orders.created", "orders.eu.created", "orders.eu.paris.created"
//   - "*.created" matches "orders.created", "payments.created"
//   - Exact match: "orders.created" matches "orders.created" only
func MatchTopic(pattern, topicName string) bool {
	// Special case: "**" matches everything
	if pattern == "**" {
		return true
	}

	// Exact match
	if pattern == topicName {
		return true
	}

	// No wildcards = must be exact match (already checked above)
	if !strings.Contains(pattern, "*") {
		return false
	}

	return matchGlob(pattern, topicName)
}

// matchGlob performs glob-style pattern matching.
// "*" matches any sequence of non-dot characters (single level)
// "**" matches any sequence of characters including dots (multi-level)
func matchGlob(pattern, name string) bool {
	// Convert glob pattern to segments for matching
	patternParts := strings.Split(pattern, ".")
	nameParts := strings.Split(name, ".")

	return matchParts(patternParts, nameParts)
}

// matchParts recursively matches pattern parts against name parts.
func matchParts(pattern, name []string) bool {
	pi, ni := 0, 0

	for pi < len(pattern) {
		if pattern[pi] == "**" {
			// "**" can match zero or more segments
			// Try matching the rest of the pattern against remaining name parts
			for ni <= len(name) {
				if matchParts(pattern[pi+1:], name[ni:]) {
					return true
				}
				ni++
			}
			return false
		}

		if ni >= len(name) {
			// Name exhausted but pattern remains
			return false
		}

		if pattern[pi] == "*" {
			// "*" matches exactly one segment (any value)
			pi++
			ni++
		} else if pattern[pi] == name[ni] {
			// Exact segment match
			pi++
			ni++
		} else {
			// No match
			return false
		}
	}

	// Both must be exhausted for a full match
	return pi == len(pattern) && ni == len(name)
}

// ConsumerCanAccess checks if a consumer has permission to access a webhook topic.
// Returns true if any of the consumer's subscriptions match the topic.
func (s *Store) ConsumerCanAccess(consumerID int64, webhookTopicName string) (bool, error) {
	subscriptions, err := s.GetConsumerSubscriptions(consumerID)
	if err != nil {
		return false, err
	}

	for _, sub := range subscriptions {
		if MatchTopic(sub.Name, webhookTopicName) {
			return true, nil
		}
	}

	return false, nil
}
