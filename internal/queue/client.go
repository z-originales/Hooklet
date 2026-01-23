package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	// ExchangeName is the topic exchange where all webhooks are published.
	// Consumers bind their queues to this exchange with specific routing keys.
	ExchangeName = "hooklet.webhooks"

	// RoutingKeyPrefix is prepended to webhook IDs to form routing keys.
	// Example: webhook ID "abc123" becomes routing key "webhook.abc123"
	RoutingKeyPrefix = "webhook."
)

// Config holds RabbitMQ queue configuration.
type Config struct {
	// MessageTTL is how long messages stay in a queue before expiring (milliseconds).
	// If a consumer doesn't read a message within this time, it's discarded.
	// Default: 300000 (5 minutes)
	MessageTTL int

	// QueueExpiry is how long an unused queue persists before auto-deletion (milliseconds).
	// If no consumer connects to a queue within this time, RabbitMQ deletes it.
	// Default: 3600000 (1 hour)
	QueueExpiry int
}

// DefaultConfig returns sensible defaults for queue configuration.
func DefaultConfig() Config {
	return Config{
		MessageTTL:  300000,  // 5 minutes - messages expire if not consumed
		QueueExpiry: 3600000, // 1 hour - queue deleted if no consumers connect
	}
}

// Client wraps RabbitMQ connection and channel.
type Client struct {
	conn    *amqp.Connection
	channel *amqp.Channel
	config  Config
}

// NewClient connects to RabbitMQ, declares the topic exchange, and returns a ready-to-use client.
func NewClient(url string, config Config) (*Client, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to open channel: %w", err)
	}

	// Declare the topic exchange (idempotent)
	// All webhooks are published to this exchange with routing key "webhook.{webhookId}"
	err = ch.ExchangeDeclare(
		ExchangeName, // name
		"topic",      // type: allows routing key patterns like "webhook.*" or "webhook.#"
		true,         // durable: survives broker restart
		false,        // auto-deleted: don't delete when no queues bound
		false,        // internal: can receive publishes from clients
		false,        // no-wait
		nil,          // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, fmt.Errorf("failed to declare exchange: %w", err)
	}

	return &Client{conn: conn, channel: ch, config: config}, nil
}

// Close cleanly shuts down the channel and connection.
func (c *Client) Close() error {
	if c.channel != nil {
		c.channel.Close()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Publish sends a webhook message to the topic exchange.
// The webhookID is used as the routing key suffix (e.g., "webhook.{webhookID}").
func (c *Client) Publish(ctx context.Context, webhookID string, body []byte) error {
	routingKey := RoutingKeyPrefix + webhookID

	err := c.channel.PublishWithContext(
		ctx,
		ExchangeName, // exchange
		routingKey,   // routing key
		false,        // mandatory
		false,        // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			Body:         body,
			DeliveryMode: amqp.Persistent, // survive broker restart
		},
	)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	return nil
}

// Subscribe creates a queue for the given consumer token and binds it to the specified webhook IDs.
// Each consumer gets their own queue (named after a hash of their token).
// Multiple consumers can subscribe to the same webhookID - each gets a copy of the message.
//
// Returns a channel of messages that will receive webhooks matching any of the bound webhookIDs.
func (c *Client) Subscribe(consumerToken string, webhookIDs []string) (<-chan amqp.Delivery, error) {
	// Generate queue name from token hash (don't expose raw tokens in RabbitMQ)
	queueName := c.queueNameForToken(consumerToken)

	// Queue arguments for TTL and expiry
	args := amqp.Table{
		"x-message-ttl": c.config.MessageTTL,  // messages expire after this duration
		"x-expires":     c.config.QueueExpiry, // queue deleted if unused for this duration
	}

	// Declare the consumer's queue
	_, err := c.channel.QueueDeclare(
		queueName,
		true,  // durable: queue survives broker restart
		false, // delete when unused: handled by x-expires instead
		false, // exclusive: allow multiple connections to same queue
		false, // no-wait
		args,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to declare queue: %w", err)
	}

	// Bind queue to exchange for each webhookID
	// This is what allows the consumer to receive specific webhooks
	for _, webhookID := range webhookIDs {
		routingKey := RoutingKeyPrefix + webhookID
		err := c.channel.QueueBind(
			queueName,    // queue name
			routingKey,   // routing key (e.g., "webhook.abc123")
			ExchangeName, // exchange
			false,        // no-wait
			nil,          // arguments
		)
		if err != nil {
			return nil, fmt.Errorf("failed to bind queue to %s: %w", routingKey, err)
		}
	}

	// Start consuming
	msgs, err := c.channel.Consume(
		queueName,
		"",    // consumer tag (auto-generated)
		true,  // auto-ack: message acknowledged on delivery
		false, // exclusive: allow multiple consumers on same queue
		false, // no-local
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		return nil, fmt.Errorf("failed to consume: %w", err)
	}

	return msgs, nil
}

// queueNameForToken generates a consistent queue name from a consumer token.
// Uses SHA256 hash to avoid exposing tokens in RabbitMQ management UI.
func (c *Client) queueNameForToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	shortHash := hex.EncodeToString(hash[:8]) // 16 chars is enough for uniqueness
	return "hooklet." + shortHash
}
