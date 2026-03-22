package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	amqp "github.com/rabbitmq/amqp091-go"
)

// ErrNoRoute is returned when a published message cannot be routed to any queue.
// This happens when no consumer has an active WebSocket connection for the topic.
var ErrNoRoute = errors.New("message not routed to any queue")

// Config holds RabbitMQ configuration.
type Config struct {
	MessageTTL  int // Milliseconds
	QueueExpiry int // Milliseconds
}

// Client wraps RabbitMQ connection and channel with automatic reconnection.
type Client struct {
	url string
	cfg Config

	mu     sync.RWMutex
	conn   *amqp.Connection
	chPool chan *pooledChannel
	closed bool

	closeCh    chan struct{} // signals Close() to reconnect goroutine
	reconnectW sync.WaitGroup
}

type pooledChannel struct {
	ch       *amqp.Channel
	returnCh chan amqp.Return
}

const (
	ExchangeName = "hooklet.webhooks"
	ExchangeType = "topic"
)

// NewClient connects to RabbitMQ and returns a ready-to-use client.
func NewClient(url string, cfg Config) (*Client, error) {
	c := &Client{
		url:     url,
		cfg:     cfg,
		closeCh: make(chan struct{}),
	}

	if err := c.connect(); err != nil {
		return nil, err
	}

	// Start reconnection goroutine
	c.reconnectW.Add(1)
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

	// We don't need this channel anymore since we use a pool
	ch.Close()

	// Initialize publisher channel pool
	poolSize := 20
	newPool := make(chan *pooledChannel, poolSize)
	for i := 0; i < poolSize; i++ {
		pch, err := conn.Channel()
		if err != nil {
			conn.Close()
			return fmt.Errorf("failed to open publisher channel: %w", err)
		}
		if err := pch.Confirm(false); err != nil {
			conn.Close()
			return fmt.Errorf("failed to enable publisher confirms: %w", err)
		}
		retCh := make(chan amqp.Return, 1)
		pch.NotifyReturn(retCh)
		newPool <- &pooledChannel{ch: pch, returnCh: retCh}
	}

	c.mu.Lock()
	// Close old resources if reconnecting
	oldPool := c.chPool
	oldConn := c.conn
	c.conn = conn
	c.chPool = newPool
	c.mu.Unlock()

	// Clean up old resources outside the lock
	if oldPool != nil {
		close(oldPool)
		for pc := range oldPool {
			pc.ch.Close()
		}
	}
	if oldConn != nil && !oldConn.IsClosed() {
		oldConn.Close()
	}

	return nil
}

// handleReconnect monitors connection and reconnects on failure.
func (c *Client) handleReconnect() {
	defer c.reconnectW.Done()

	for {
		c.mu.RLock()
		conn := c.conn
		c.mu.RUnlock()

		if conn == nil {
			return
		}

		// Register for both connection and channel close notifications
		connClose := make(chan *amqp.Error, 1)
		conn.NotifyClose(connClose)

		// Wait for connection failure, or explicit close
		select {
		case <-c.closeCh:
			return
		case err := <-connClose:
			if err == nil {
				return // graceful close
			}
			log.Warn("RabbitMQ connection lost, reconnecting...", "error", err)
		}

		// Exponential backoff reconnection
		backoff := time.Second
		maxBackoff := 30 * time.Second

		for {
			select {
			case <-c.closeCh:
				return
			case <-time.After(backoff):
			}

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
// It waits for the reconnect goroutine to exit.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.closeCh)
	pool := c.chPool
	conn := c.conn
	c.mu.Unlock()

	// Wait for reconnect goroutine to exit
	c.reconnectW.Wait()

	if pool != nil {
		close(pool)
		for pc := range pool {
			pc.ch.Close()
		}
	}
	if conn != nil {
		if err := conn.Close(); err != nil {
			return fmt.Errorf("failed to close connection: %w", err)
		}
	}
	return nil
}

// Publish sends a message to the topic exchange.
// Messages are persistent and will survive RabbitMQ restarts (until TTL expires).
func (c *Client) Publish(ctx context.Context, topic string, body []byte) error {
	c.mu.RLock()
	pool := c.chPool
	c.mu.RUnlock()

	if pool == nil {
		return fmt.Errorf("not connected to RabbitMQ")
	}

	var pc *pooledChannel
	select {
	case pc = <-pool:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Make sure we return the channel to the pool
	defer func() {
		select {
		case pool <- pc:
		default:
			pc.ch.Close()
		}
	}()

	// Drain any stale returns from previous uses of this channel
	select {
	case <-pc.returnCh:
	default:
	}

	routingKey := fmt.Sprintf("webhook.%s", topic)

	dc, err := pc.ch.PublishWithDeferredConfirmWithContext(
		ctx,
		ExchangeName,
		routingKey,
		true,  // mandatory: wait for return to know if there's no consumer bound
		false, // immediate
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to publish message: %w", err)
	}

	// Wait for broker confirmation asynchronously from other publishers
	ok, err := dc.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to confirm message: %w", err)
	}
	if !ok {
		return fmt.Errorf("message was nacked by broker")
	}

	// Check if the message was returned (no bound queue for this routing key).
	// With mandatory=true, RabbitMQ sends basic.return before basic.ack,
	// so the return is already in the channel by the time the confirm arrives.
	select {
	case <-pc.returnCh:
		return ErrNoRoute
	default:
		return nil
	}
}

// Subscribe creates a dedicated queue for the consumer and binds it to the requested topics.
// Queues are durable and will survive RabbitMQ restarts, allowing consumers to
// reconnect and retrieve messages that arrived while they were offline (within TTL).
// The caller must cancel the context to stop consuming and release the AMQP channel.
func (c *Client) Subscribe(ctx context.Context, consumerID string, topics []string) (<-chan amqp.Delivery, error) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil || conn.IsClosed() {
		return nil, fmt.Errorf("not connected to RabbitMQ")
	}

	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("failed to open channel: %w", err)
	}

	// Declare a durable queue for this consumer
	// The queue persists across RabbitMQ restarts and consumer reconnections
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
		true,  // durable: queue survives broker restart
		false, // delete when unused: keep queue for reconnecting consumers
		false, // exclusive: allow reconnections from same consumer
		false, // no-wait
		args,
	)
	if err != nil {
		ch.Close()
		return nil, fmt.Errorf("failed to declare consumer queue: %w", err)
	}

	// Bind the queue to the exchange for each topic
	for _, topic := range topics {
		routingKey := fmt.Sprintf("webhook.%s", normalizeTopicPattern(topic))
		err := ch.QueueBind(
			q.Name,
			routingKey,
			ExchangeName,
			false,
			nil,
		)
		if err != nil {
			if _, deleteErr := ch.QueueDelete(queueName, false, false, false); deleteErr != nil {
				log.Warn("Failed to delete queue after bind error", "queue", queueName, "error", deleteErr)
			}
			if closeErr := ch.Close(); closeErr != nil {
				log.Warn("Failed to close channel after bind error", "queue", queueName, "error", closeErr)
			}
			return nil, fmt.Errorf("failed to bind topic %s: %w", topic, err)
		}
	}

	// Start consuming
	msgs, err := ch.Consume(
		q.Name,
		"",    // consumer tag (auto-generated)
		false, // auto-ack: require explicit acknowledgement
		false, // exclusive: allow multiple consumers (for reconnection)
		false, // no-local
		false, // no-wait
		nil,
	)
	if err != nil {
		ch.Close()
		return nil, fmt.Errorf("failed to consume: %w", err)
	}

	out := make(chan amqp.Delivery)
	go func() {
		defer func() {
			if err := ch.Close(); err != nil {
				log.Debug("Failed to close consumer channel", "consumer", consumerID, "error", err)
			}
		}()
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-msgs:
				if !ok {
					return
				}
				select {
				case out <- msg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

// normalizeTopicPattern converts Hooklet patterns to AMQP topic patterns.
// Hooklet: "*" matches one level, "**" matches all levels.
// AMQP: "*" matches one word, "#" matches zero or more words.
func normalizeTopicPattern(topic string) string {
	return strings.ReplaceAll(topic, "**", "#")
}
