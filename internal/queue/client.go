package queue

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Config holds RabbitMQ configuration.
type Config struct {
	MessageTTL  int // Milliseconds
	QueueExpiry int // Milliseconds
}

// Client wraps RabbitMQ connection and channel.
type Client struct {
	conn    *amqp.Connection
	channel *amqp.Channel
	cfg     Config
}

const (
	ExchangeName = "hooklet.webhooks"
	ExchangeType = "topic"
)

// NewClient connects to RabbitMQ and returns a ready-to-use client.
func NewClient(url string, cfg Config) (*Client, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to open channel: %w", err)
	}

	// Declare the Topic Exchange immediately
	err = ch.ExchangeDeclare(
		ExchangeName, // name
		ExchangeType, // type
		true,         // durable
		false,        // auto-deleted
		false,        // internal
		false,        // no-wait
		nil,          // arguments
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, fmt.Errorf("failed to declare exchange: %w", err)
	}

	return &Client{conn: conn, channel: ch, cfg: cfg}, nil
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

// Publish sends a message to the topic exchange.
func (c *Client) Publish(ctx context.Context, topic string, body []byte) error {
	// Routing key format: webhook.<topic>
	routingKey := fmt.Sprintf("webhook.%s", topic)

	err := c.channel.PublishWithContext(
		ctx,
		ExchangeName, // exchange
		routingKey,   // routing key
		false,        // mandatory
		false,        // immediate
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
	// 1. Declare a unique, auto-delete queue for this consumer
	queueName := fmt.Sprintf("hooklet-ws-%s", consumerID)

	args := amqp.Table{}
	if c.cfg.MessageTTL > 0 {
		args["x-message-ttl"] = c.cfg.MessageTTL
	}
	if c.cfg.QueueExpiry > 0 {
		args["x-expires"] = c.cfg.QueueExpiry
	}

	q, err := c.channel.QueueDeclare(
		queueName,
		false, // durable (false = transient, fits WS clients)
		true,  // delete when unused (true = cleanup on disconnect)
		true,  // exclusive (true = only this connection can use it)
		false, // no-wait
		args,  // arguments (TTL, Expiry)
	)
	if err != nil {
		return nil, fmt.Errorf("failed to declare consumer queue: %w", err)
	}

	// 2. Bind the queue to the exchange for each topic
	for _, topic := range topics {
		routingKey := fmt.Sprintf("webhook.%s", topic)
		err := c.channel.QueueBind(
			q.Name,       // queue name
			routingKey,   // routing key
			ExchangeName, // exchange
			false,
			nil,
		)
		if err != nil {
			// Try cleanup
			c.channel.QueueDelete(queueName, false, false, false)
			return nil, fmt.Errorf("failed to bind topic %s: %w", topic, err)
		}
	}

	// 3. Start consuming
	msgs, err := c.channel.Consume(
		q.Name,
		"",    // consumer tag (auto-generated)
		false, // auto-ack (false = we ack manually after sending to WS)
		true,  // exclusive
		false, // no-local
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		return nil, fmt.Errorf("failed to consume: %w", err)
	}

	return msgs, nil
}
