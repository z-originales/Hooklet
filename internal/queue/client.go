package queue

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Config holds RabbitMQ configuration.
type Config struct {
	MessageTTL  int // Milliseconds
	QueueExpiry int // Milliseconds
}

// Client wraps RabbitMQ connection and channel with automatic reconnection.
type Client struct {
	url string
	cfg Config

	mu      sync.RWMutex
	conn    *amqp.Connection
	channel *amqp.Channel
	closed  bool

	notifyClose chan *amqp.Error
}

const (
	ExchangeName = "hooklet.webhooks"
	ExchangeType = "topic"
)

// NewClient connects to RabbitMQ and returns a ready-to-use client.
func NewClient(url string, cfg Config) (*Client, error) {
	c := &Client{
		url: url,
		cfg: cfg,
	}

	if err := c.connect(); err != nil {
		return nil, err
	}

	// Start reconnection goroutine
	go c.handleReconnect()

	return c, nil
}

// connect establishes connection and channel to RabbitMQ.
func (c *Client) connect() error {
	conn, err := amqp.Dial(c.url)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to open channel: %w", err)
	}

	// Declare the Topic Exchange
	err = ch.ExchangeDeclare(
		ExchangeName,
		ExchangeType,
		true,  // durable
		false, // auto-deleted
		false, // internal
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return fmt.Errorf("failed to declare exchange: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.channel = ch
	c.notifyClose = make(chan *amqp.Error, 1)
	c.conn.NotifyClose(c.notifyClose)
	c.mu.Unlock()

	return nil
}

// handleReconnect monitors connection and reconnects on failure.
func (c *Client) handleReconnect() {
	for {
		c.mu.RLock()
		if c.closed {
			c.mu.RUnlock()
			return
		}
		notifyClose := c.notifyClose
		c.mu.RUnlock()

		// Wait for connection close notification
		err := <-notifyClose
		if err == nil {
			// Graceful close (client shutdown), exit
			return
		}

		log.Warn("RabbitMQ connection lost, reconnecting...", "error", err)

		// Exponential backoff reconnection
		backoff := time.Second
		maxBackoff := 30 * time.Second

		for {
			c.mu.RLock()
			if c.closed {
				c.mu.RUnlock()
				return
			}
			c.mu.RUnlock()

			time.Sleep(backoff)

			if err := c.connect(); err != nil {
				log.Error("Reconnection failed", "error", err, "retry_in", backoff)
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}

			log.Info("RabbitMQ reconnected successfully")
			break
		}
	}
}

// IsConnected returns true if the client is connected to RabbitMQ.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn != nil && !c.conn.IsClosed()
}

// Close cleanly shuts down the channel and connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true

	if c.channel != nil {
		c.channel.Close()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Publish sends a message to the topic exchange.
func (c *Client) Publish(ctx context.Context, topic string, body []byte) error {
	c.mu.RLock()
	ch := c.channel
	c.mu.RUnlock()

	if ch == nil {
		return fmt.Errorf("not connected to RabbitMQ")
	}

	routingKey := fmt.Sprintf("webhook.%s", topic)

	err := ch.PublishWithContext(
		ctx,
		ExchangeName,
		routingKey,
		false, // mandatory
		false, // immediate
		amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	return nil
}

// Subscribe creates a dedicated queue for the consumer and binds it to the requested topics.
func (c *Client) Subscribe(consumerID string, topics []string) (<-chan amqp.Delivery, error) {
	c.mu.RLock()
	ch := c.channel
	c.mu.RUnlock()

	if ch == nil {
		return nil, fmt.Errorf("not connected to RabbitMQ")
	}

	// Declare a unique, auto-delete queue for this consumer
	queueName := fmt.Sprintf("hooklet-ws-%s", consumerID)

	args := amqp.Table{}
	if c.cfg.MessageTTL > 0 {
		args["x-message-ttl"] = c.cfg.MessageTTL
	}
	if c.cfg.QueueExpiry > 0 {
		args["x-expires"] = c.cfg.QueueExpiry
	}

	q, err := ch.QueueDeclare(
		queueName,
		false, // durable
		true,  // delete when unused
		true,  // exclusive
		false, // no-wait
		args,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to declare consumer queue: %w", err)
	}

	// Bind the queue to the exchange for each topic
	for _, topic := range topics {
		routingKey := fmt.Sprintf("webhook.%s", topic)
		err := ch.QueueBind(
			q.Name,
			routingKey,
			ExchangeName,
			false,
			nil,
		)
		if err != nil {
			ch.QueueDelete(queueName, false, false, false)
			return nil, fmt.Errorf("failed to bind topic %s: %w", topic, err)
		}
	}

	// Start consuming
	msgs, err := ch.Consume(
		q.Name,
		"",    // consumer tag (auto-generated)
		false, // auto-ack
		true,  // exclusive
		false, // no-local
		false, // no-wait
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to consume: %w", err)
	}

	return msgs, nil
}
