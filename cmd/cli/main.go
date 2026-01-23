package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	Status  StatusCmd  `cmd:"" help:"Check service status"`
	Webhook WebhookCmd `cmd:"" help:"Manage webhooks"`
	User    UserCmd    `cmd:"" help:"Manage users"`
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
		socketPath = api.DefaultSocketPath
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
	Create WebhookCreateCmd `cmd:"" help:"Create a new webhook"`
	List   WebhookListCmd   `cmd:"" help:"List all webhooks"`
	Delete WebhookDeleteCmd `cmd:"" help:"Delete a webhook"`
}

type WebhookCreateCmd struct {
	Name string `arg:"" help:"Name of the webhook"`
}

func (c *WebhookCreateCmd) Run(ctx *Context) error {
	resp, err := ctx.adminRequest(http.MethodPost, "/admin/webhooks", map[string]string{"name": c.Name})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	var wh store.Webhook
	if err := json.NewDecoder(resp.Body).Decode(&wh); err != nil {
		return err
	}

	fmt.Printf("Webhook created!\n")
	fmt.Printf("  Name:      %s\n", wh.Name)
	fmt.Printf("  ID:        %d\n", wh.ID)
	fmt.Printf("  Topic URL: /webhook/%s\n", wh.TopicHash)
	fmt.Println("")
	fmt.Println("Use the Topic URL to publish webhooks. The hash prevents topic enumeration.")
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
		fmt.Printf("  [%d] %s\n", w.ID, w.Name)
		fmt.Printf("      URL: /webhook/%s\n", w.TopicHash)
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

// User Commands
type UserCmd struct {
	Create UserCreateCmd `cmd:"" help:"Create a new user"`
	List   UserListCmd   `cmd:"" help:"List all users"`
}

type UserCreateCmd struct {
	Name          string `arg:"" help:"Name of the user"`
	Subscriptions string `help:"Comma separated list of subscribed topics (or *)" default:""`
}

func (c *UserCreateCmd) Run(ctx *Context) error {
	req := map[string]string{
		"name":          c.Name,
		"subscriptions": c.Subscriptions,
	}
	resp, err := ctx.adminRequest(http.MethodPost, "/admin/users", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	var res struct {
		store.User
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}

	fmt.Printf("User created: %s (ID: %d)\n", res.Name, res.ID)
	fmt.Printf("Token: %s\n", res.Token)
	fmt.Println("SAVE THIS TOKEN! It will not be shown again.")
	return nil
}

type UserListCmd struct{}

func (c *UserListCmd) Run(ctx *Context) error {
	resp, err := ctx.adminRequest(http.MethodGet, "/admin/users", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed: %s", body)
	}

	var users []store.User
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return err
	}

	fmt.Println("Users:")
	for _, u := range users {
		fmt.Printf("  %d: %s (Subs: %s)\n", u.ID, u.Name, u.Subscriptions)
	}
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
