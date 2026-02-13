package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hooklet/internal/config"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"hooklet/internal/api"
	"hooklet/internal/store"

	"github.com/alecthomas/kong"
	"github.com/charmbracelet/log"
)

// CLI defines the command-line interface structure.
var CLI struct {
	// Global flags
	Host       string `help:"Service host (defaults to Unix socket if available)" default:"" env:"HOOKLET_HOST"`
	Port       string `help:"Service port (only used if host is set)" default:"8080" env:"HOOKLET_PORT"`
	AdminToken string `help:"Admin token for management commands" env:"HOOKLET_ADMIN_TOKEN"`
	Socket     string `help:"Unix socket path" env:"HOOKLET_SOCKET"`

	// Commands (Administration only - no publish/subscribe)
	Status   StatusCmd   `cmd:"" help:"Check service status"`
	Webhook  WebhookCmd  `cmd:"" help:"Manage webhooks"`
	Consumer ConsumerCmd `cmd:"" help:"Manage consumers"`
}

// Context holds shared CLI context.
type Context struct {
	Host       string
	Port       string
	AdminToken string
	Socket     string
	client     *http.Client
}

// getClient returns an HTTP client configured for either Unix Socket or TCP
func (c *Context) getClient() *http.Client {
	if c.client != nil {
		return c.client
	}

	// If Host is explicitly set, use standard HTTP client
	if c.Host != "" {
		c.client = http.DefaultClient
		return c.client
	}

	// Otherwise, use Unix Socket
	socketPath := c.Socket
	if socketPath == "" {
		socketPath = config.DefaultSocketPath
	}

	// Custom Transport for Unix Socket
	c.client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
	return c.client
}

func (c *Context) baseURL() string {
	// If Host is set, use TCP URL
	if c.Host != "" {
		return fmt.Sprintf("http://%s:%s", c.Host, c.Port)
	}
	// For Unix Socket, the host part is ignored by our custom DialContext
	// but we need a valid URL structure.
	return "http://unix"
}

func (c *Context) adminRequest(method, path string, body any) (*http.Response, error) {
	url := c.baseURL() + path
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	// Add token only if we have one, but for local socket it's optional
	if c.AdminToken != "" {
		req.Header.Set("X-Hooklet-Admin-Token", c.AdminToken)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.getClient().Do(req)
}

// Webhook Commands
type WebhookCmd struct {
	Create     WebhookCreateCmd     `cmd:"" help:"Create a new webhook"`
	List       WebhookListCmd       `cmd:"" help:"List all webhooks"`
	Delete     WebhookDeleteCmd     `cmd:"" help:"Delete a webhook"`
	SetToken   WebhookSetTokenCmd   `cmd:"" help:"Generate/regenerate auth token for a webhook"`
	ClearToken WebhookClearTokenCmd `cmd:"" help:"Remove auth token from a webhook (disable producer auth)"`
}

type WebhookCreateCmd struct {
	Name      string `arg:"" help:"Name of the webhook"`
	WithToken bool   `help:"Generate an authentication token for producers" default:"false"`
}

func (c *WebhookCreateCmd) Run(ctx *Context) error {
	req := map[string]interface{}{
		"name":       c.Name,
		"with_token": c.WithToken,
	}
	resp, err := ctx.adminRequest(http.MethodPost, "/admin/webhooks", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	// Response may include token if with_token was true
	var result struct {
		store.Webhook
		Token string `json:"token,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	fmt.Printf("Webhook created!\n")
	fmt.Printf("  Name:      %s\n", result.Name)
	fmt.Printf("  ID:        %d\n", result.ID)
	fmt.Printf("  Topic URL: /webhook/%s\n", result.TopicHash)
	fmt.Printf("  Auth:      %s\n", map[bool]string{true: "required", false: "none"}[result.HasToken])

	if result.Token != "" {
		fmt.Printf("\n  Token: %s\n", result.Token)
		fmt.Println("\n  SAVE THIS TOKEN! It will NOT be shown again.")
		fmt.Println("  Producers must send it via X-Hooklet-Token header.")
	} else {
		fmt.Println("\nUse the Topic URL to publish webhooks. The hash prevents topic enumeration.")
	}
	return nil
}

type WebhookListCmd struct{}

func (c *WebhookListCmd) Run(ctx *Context) error {
	resp, err := ctx.adminRequest(http.MethodGet, "/admin/webhooks", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	var webhooks []store.Webhook
	if err := json.NewDecoder(resp.Body).Decode(&webhooks); err != nil {
		return err
	}

	if len(webhooks) == 0 {
		fmt.Println("No webhooks registered.")
		return nil
	}

	fmt.Println("Webhooks:")
	for _, w := range webhooks {
		authStatus := "none"
		if w.HasToken {
			authStatus = "token required"
		}
		fmt.Printf("  [%d] %s\n", w.ID, w.Name)
		fmt.Printf("      URL: /webhook/%s\n", w.TopicHash)
		fmt.Printf("      Auth: %s\n", authStatus)
		fmt.Printf("      Created: %s\n", w.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

type WebhookDeleteCmd struct {
	ID int64 `arg:"" help:"ID of the webhook to delete"`
}

func (c *WebhookDeleteCmd) Run(ctx *Context) error {
	path := fmt.Sprintf("/admin/webhooks/%d", c.ID)
	resp, err := ctx.adminRequest(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	fmt.Printf("Webhook %d deleted\n", c.ID)
	return nil
}

// WebhookSetTokenCmd generates/regenerates an auth token for a webhook.
type WebhookSetTokenCmd struct {
	ID int64 `arg:"" help:"ID of the webhook"`
}

func (c *WebhookSetTokenCmd) Run(ctx *Context) error {
	path := fmt.Sprintf("/admin/webhooks/%d/set-token", c.ID)
	resp, err := ctx.adminRequest(http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	var result struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	fmt.Printf("Token generated for webhook '%s' (ID: %d)\n", result.Name, result.ID)
	fmt.Printf("\n  Token: %s\n", result.Token)
	fmt.Println("\n  SAVE THIS TOKEN! It will NOT be shown again.")
	fmt.Println("  Producers must send it via X-Hooklet-Token header.")
	return nil
}

// WebhookClearTokenCmd removes the auth token from a webhook.
type WebhookClearTokenCmd struct {
	ID int64 `arg:"" help:"ID of the webhook"`
}

func (c *WebhookClearTokenCmd) Run(ctx *Context) error {
	path := fmt.Sprintf("/admin/webhooks/%d/clear-token", c.ID)
	resp, err := ctx.adminRequest(http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	fmt.Printf("Token removed from webhook %d\n", c.ID)
	fmt.Println("This webhook no longer requires producer authentication.")
	return nil
}

// Consumer Commands
type ConsumerCmd struct {
	Create      ConsumerCreateCmd      `cmd:"" help:"Create a new consumer"`
	List        ConsumerListCmd        `cmd:"" help:"List all consumers"`
	Delete      ConsumerDeleteCmd      `cmd:"" help:"Delete a consumer"`
	Subscribe   ConsumerSubscribeCmd   `cmd:"" help:"Add a topic subscription to a consumer"`
	Unsubscribe ConsumerUnsubscribeCmd `cmd:"" help:"Remove a topic subscription from a consumer"`
	SetSubs     ConsumerSetSubsCmd     `cmd:"" help:"Replace all subscriptions for a consumer"`
	RegenToken  ConsumerRegenTokenCmd  `cmd:"" help:"Regenerate consumer token"`
}

type ConsumerCreateCmd struct {
	Name          string `arg:"" help:"Name of the consumer"`
	Subscriptions string `help:"Comma-separated list of topic subscriptions (use '**' for all topics)" default:""`
}

func (c *ConsumerCreateCmd) Run(ctx *Context) error {
	req := map[string]string{
		"name":          c.Name,
		"subscriptions": c.Subscriptions,
	}
	resp, err := ctx.adminRequest(http.MethodPost, "/admin/consumers", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	var res struct {
		store.Consumer
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}

	fmt.Printf("Consumer created: %s (ID: %d)\n", res.Name, res.ID)
	fmt.Printf("Token: %s\n", res.Token)
	fmt.Println("SAVE THIS TOKEN! It will not be shown again.")
	return nil
}

type ConsumerListCmd struct{}

func (c *ConsumerListCmd) Run(ctx *Context) error {
	resp, err := ctx.adminRequest(http.MethodGet, "/admin/consumers", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	var consumers []store.Consumer
	if err := json.NewDecoder(resp.Body).Decode(&consumers); err != nil {
		return err
	}

	fmt.Println("Consumers:")
	for _, c := range consumers {
		fmt.Printf("  [%d] %s\n", c.ID, c.Name)
		fmt.Printf("      Created: %s\n", c.CreatedAt.Format(time.RFC3339))
	}
	fmt.Println("\nUse 'hooklet consumer subscribe <id> --topic <pattern>' to add subscriptions.")
	fmt.Println("Tip: Use '**' pattern to subscribe to all topics.")
	return nil
}

type ConsumerDeleteCmd struct {
	ID int64 `arg:"" help:"ID of the consumer to delete"`
}

func (c *ConsumerDeleteCmd) Run(ctx *Context) error {
	path := fmt.Sprintf("/admin/consumers/%d", c.ID)
	resp, err := ctx.adminRequest(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	fmt.Printf("Consumer %d deleted\n", c.ID)
	return nil
}

// ConsumerSubscribeCmd adds a single topic subscription to a consumer.
type ConsumerSubscribeCmd struct {
	ID    int64  `arg:"" help:"ID of the consumer"`
	Topic string `help:"Topic pattern to subscribe to (e.g., 'orders.*', '**' for all)" required:""`
}

func (c *ConsumerSubscribeCmd) Run(ctx *Context) error {
	path := fmt.Sprintf("/admin/consumers/%d/subscribe", c.ID)
	req := map[string]string{"topic": c.Topic}
	resp, err := ctx.adminRequest(http.MethodPost, path, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	fmt.Printf("Consumer %d subscribed to: %s\n", c.ID, c.Topic)
	if c.Topic == "**" {
		fmt.Println("This consumer now has access to ALL topics.")
	}
	return nil
}

// ConsumerUnsubscribeCmd removes a topic subscription from a consumer.
type ConsumerUnsubscribeCmd struct {
	ID    int64  `arg:"" help:"ID of the consumer"`
	Topic string `help:"Topic pattern to unsubscribe from" required:""`
}

func (c *ConsumerUnsubscribeCmd) Run(ctx *Context) error {
	path := fmt.Sprintf("/admin/consumers/%d/unsubscribe", c.ID)
	req := map[string]string{"topic": c.Topic}
	resp, err := ctx.adminRequest(http.MethodPost, path, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	fmt.Printf("Consumer %d unsubscribed from: %s\n", c.ID, c.Topic)
	return nil
}

// ConsumerSetSubsCmd replaces all subscriptions for a consumer.
type ConsumerSetSubsCmd struct {
	ID            int64  `arg:"" help:"ID of the consumer"`
	Subscriptions string `help:"Comma-separated list of topic patterns (use '**' for all topics)" required:""`
}

func (c *ConsumerSetSubsCmd) Run(ctx *Context) error {
	path := fmt.Sprintf("/admin/consumers/%d", c.ID)
	req := map[string]string{"subscriptions": c.Subscriptions}
	resp, err := ctx.adminRequest(http.MethodPatch, path, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	fmt.Printf("Consumer %d subscriptions set to: %s\n", c.ID, c.Subscriptions)
	if c.Subscriptions == "**" {
		fmt.Println("This consumer now has access to ALL topics.")
	}
	return nil
}

type ConsumerRegenTokenCmd struct {
	ID int64 `arg:"" help:"ID of the consumer"`
}

func (c *ConsumerRegenTokenCmd) Run(ctx *Context) error {
	path := fmt.Sprintf("/admin/consumers/%d", c.ID)
	req := map[string]bool{"regenerate_token": true}
	resp, err := ctx.adminRequest(http.MethodPatch, path, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	var res struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}

	fmt.Printf("New token: %s\n", res.Token)
	fmt.Println("SAVE THIS TOKEN! It will not be shown again.")
	return nil
}

// StatusCmd checks the service health.
type StatusCmd struct{}

func (c *StatusCmd) Run(ctx *Context) error {
	url := ctx.baseURL() + api.RouteStatus

	resp, err := ctx.getClient().Get(url)
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

func main() {
	log.SetLevel(log.WarnLevel) // Quiet by default for CLI

	kctx := kong.Parse(&CLI,
		kong.Name("hooklet"),
		kong.Description("CLI client for hooklet webhook service"),
		kong.ConfigureHelp(kong.HelpOptions{
			NoExpandSubcommands: true,
			Compact:             true,
		}),
		kong.UsageOnError(),
	)

	ctx := &Context{
		Host:       CLI.Host,
		Port:       CLI.Port,
		AdminToken: CLI.AdminToken,
		Socket:     CLI.Socket,
	}

	err := kctx.Run(ctx)
	if err != nil {
		log.Error("Command failed", "error", err)
		os.Exit(1)
	}
}
