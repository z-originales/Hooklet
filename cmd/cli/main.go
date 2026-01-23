// CLI is the command-line client for interacting with the hooklet service.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"hooklet/internal/api"

	"github.com/alecthomas/kong"
	"github.com/charmbracelet/log"
	"github.com/coder/websocket"
)

// CLI defines the command-line interface structure.
var CLI struct {
	// Global flags
	Host string `help:"Service host" default:"localhost" env:"HOOKLET_HOST"`
	Port string `help:"Service port" default:"8080" env:"HOOKLET_PORT"`

	// TODO: Add authentication token flag when implementing security
	// Token string `help:"Authentication token" env:"HOOKLET_TOKEN"`

	// Commands
	Status    StatusCmd    `cmd:"" help:"Check service status"`
	Topics    TopicsCmd    `cmd:"" help:"List active topics"`
	Publish   PublishCmd   `cmd:"" help:"Publish a message to a topic"`
	Subscribe SubscribeCmd `cmd:"" help:"Subscribe to a topic and stream messages"`
}

// StatusCmd checks the service health.
type StatusCmd struct{}

func (c *StatusCmd) Run(ctx *Context) error {
	url := ctx.baseURL() + api.RouteStatus

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to connect to service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("service error: %s", body)
	}

	var status api.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	fmt.Printf("Status:    %s\n", status.Status)
	fmt.Printf("Uptime:    %s\n", status.Uptime)
	fmt.Printf("Started:   %s\n", status.StartedAt.Format(time.RFC3339))
	fmt.Printf("RabbitMQ:  %s\n", status.RabbitMQ)

	return nil
}

// TopicsCmd lists active topics.
type TopicsCmd struct{}

func (c *TopicsCmd) Run(ctx *Context) error {
	url := ctx.baseURL() + api.RouteTopics

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to connect to service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("service error: %s", body)
	}

	var topics api.TopicsResponse
	if err := json.NewDecoder(resp.Body).Decode(&topics); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if len(topics.Topics) == 0 {
		fmt.Println("No active topics")
		return nil
	}

	fmt.Println("Active topics:")
	for _, topic := range topics.Topics {
		fmt.Printf("  - %s\n", topic)
	}

	return nil
}

// PublishCmd publishes a message to a topic.
type PublishCmd struct {
	Topic   string `arg:"" help:"Topic to publish to"`
	Message string `arg:"" optional:"" help:"Message to publish (reads from stdin if not provided)"`
	File    string `short:"f" help:"Read message from file"`
}

func (c *PublishCmd) Run(ctx *Context) error {
	var payload []byte
	var err error

	// Determine message source
	switch {
	case c.File != "":
		payload, err = os.ReadFile(c.File)
		if err != nil {
			return fmt.Errorf("failed to read file: %w", err)
		}
	case c.Message != "":
		payload = []byte(c.Message)
	default:
		// Read from stdin
		payload, err = io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read stdin: %w", err)
		}
	}

	if len(payload) == 0 {
		return fmt.Errorf("empty message")
	}

	url := ctx.baseURL() + api.RoutePublish + c.Topic

	// TODO: Add authentication header when implementing security
	// req.Header.Set(api.HeaderAuthToken, ctx.Token)

	resp, err := http.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to publish: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("publish failed: %s", body)
	}

	fmt.Printf("Published %d bytes to topic '%s'\n", len(payload), c.Topic)
	return nil
}

// SubscribeCmd subscribes to topics and streams messages.
type SubscribeCmd struct {
	Topics []string `arg:"" help:"Topics to subscribe to (comma-separated)"`
	Raw    bool     `short:"r" help:"Output raw messages without formatting"`
}

func (c *SubscribeCmd) Run(ctx *Context) error {
	// Build WebSocket URL
	// ws://host:port/ws/?topics=t1,t2
	u := fmt.Sprintf("ws://%s:%s%s", ctx.Host, ctx.Port, api.RouteSubscribe)

	// Append topics as query params
	if len(c.Topics) > 0 {
		// Only use query param if topics are provided via args
		// (API also supports /ws/{topic} but we prefer query params now)
		u += "?" + api.QueryParamTopics + "=" + strings.Join(c.Topics, ",")
	} else {
		return fmt.Errorf("at least one topic is required")
	}

	// TODO: Add TLS support for production (wss://)
	// TODO: Add authentication via query param or initial message

	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle interrupt signal for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nDisconnecting...")
		cancel()
	}()

	// Connect to WebSocket
	conn, _, err := websocket.Dial(bgCtx, u, nil)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	if !c.Raw {
		fmt.Printf("Subscribed to topics '%v' (Ctrl+C to exit)\n", c.Topics)
		fmt.Println(strings.Repeat("-", 40))
	}

	// Read messages
	for {
		_, msg, err := conn.Read(bgCtx)
		if err != nil {
			if bgCtx.Err() != nil {
				// Context cancelled (user interrupt)
				return nil
			}
			return fmt.Errorf("connection error: %w", err)
		}

		if c.Raw {
			fmt.Println(string(msg))
		} else {
			// Pretty print with timestamp
			fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), string(msg))
		}
	}
}

// Context holds shared CLI context.
type Context struct {
	Host string
	Port string
	// TODO: Token string
}

func (c *Context) baseURL() string {
	return fmt.Sprintf("http://%s:%s", c.Host, c.Port)
}

func main() {
	log.SetLevel(log.WarnLevel) // Quiet by default for CLI

	kctx := kong.Parse(&CLI,
		kong.Name("hooklet"),
		kong.Description("CLI client for hooklet webhook service"),
		kong.UsageOnError(),
	)

	ctx := &Context{
		Host: CLI.Host,
		Port: CLI.Port,
	}

	err := kctx.Run(ctx)
	if err != nil {
		log.Error("Command failed", "error", err)
		os.Exit(1)
	}
}
