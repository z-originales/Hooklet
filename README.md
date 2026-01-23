# Hooklet

A lightweight, secure webhook relay that lets you receive webhooks on a public server and stream them to your private applications via WebSocket.

## The Problem

Receiving webhooks locally or on internal services requires either:
- Exposing your machine's port to the internet (risky)
- Complex tunneling solutions like ngrok (dependencies, cost)

## The Solution

**Deport your webhook endpoint.** Host Hooklet on a small VPS. It accepts incoming webhooks and streams them in real-time to your applications via outbound WebSocket connections. No inbound ports needed on your private infrastructure.

```
[Stripe/GitHub/etc] --POST--> [Hooklet on VPS] --WebSocket--> [Your App behind NAT]
```

## Features

- **Secure by Default**: Topics must be registered before use, users need tokens
- **Topic Hashing**: Webhook URLs use SHA256 hashes to prevent enumeration
- **Real-Time Streaming**: Instant delivery via WebSocket
- **Multi-Topic Subscriptions**: Listen to multiple webhooks on one connection
- **Reliable Delivery**: Built on RabbitMQ with message persistence
- **Simple Administration**: CLI tool for managing webhooks and users

## Quick Start

### Prerequisites

- Go 1.21+
- RabbitMQ (via Docker or installed)

### 1. Start RabbitMQ

```bash
docker-compose up -d
```

### 2. Build & Run the Service

```bash
# Build
go build -o hooklet-service ./cmd/service
go build -o hooklet-cli ./cmd/cli

# Run
./hooklet-service
```

### 3. Create a Webhook (Admin)

```bash
./hooklet-cli webhook create stripe-payments
```

Output:
```
Webhook created!
  Name:      stripe-payments
  ID:        1
  Topic URL: /webhook/a1b2c3d4e5f6...

Use the Topic URL to publish webhooks. The hash prevents topic enumeration.
```

### 4. Create a User (Admin)

```bash
./hooklet-cli user create my-backend --subscriptions=stripe-payments
```

Output:
```
User created: my-backend (ID: 1)
Token: my-backend-1737012345678
SAVE THIS TOKEN! It will not be shown again.
```

### 5. Configure Your Webhook Provider

Point Stripe (or any service) to:
```
https://your-server.com/webhook/a1b2c3d4e5f6...
```

### 6. Connect Your Application

Your app connects via WebSocket and authenticates:

```
1. Connect:  ws://your-server.com/ws?topics=stripe-payments
2. Send:     {"type":"auth","token":"my-backend-1737012345678"}
3. Receive:  {"type":"auth_ok","user":"my-backend"}
4. Stream:   ... webhook payloads arrive here ...
```

---

## Client Examples

### Go

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/coder/websocket"
)

func main() {
	ctx := context.Background()

	// Connect to Hooklet
	conn, _, err := websocket.Dial(ctx, "ws://localhost:8080/ws?topics=stripe-payments", nil)
	if err != nil {
		log.Fatal("Connection failed:", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Authenticate (token in message body, not URL - prevents log leakage)
	auth := map[string]string{"type": "auth", "token": "my-backend-1737012345678"}
	authBytes, _ := json.Marshal(auth)
	if err := conn.Write(ctx, websocket.MessageText, authBytes); err != nil {
		log.Fatal("Auth failed:", err)
	}

	// Wait for auth confirmation
	_, resp, _ := conn.Read(ctx)
	fmt.Printf("Auth response: %s\n", resp)

	// Listen for webhooks
	fmt.Println("Listening for webhooks...")
	for {
		_, message, err := conn.Read(ctx)
		if err != nil {
			log.Println("Connection closed:", err)
			return
		}
		fmt.Printf("Webhook received: %s\n", message)
	}
}
```

### Python

```python
import asyncio
import json
import websockets

async def listen():
    uri = "ws://localhost:8080/ws?topics=stripe-payments"
    
    async with websockets.connect(uri) as ws:
        # Authenticate
        await ws.send(json.dumps({
            "type": "auth",
            "token": "my-backend-1737012345678"
        }))
        
        # Wait for confirmation
        response = await ws.recv()
        print(f"Auth: {response}")
        
        # Listen for webhooks
        print("Listening for webhooks...")
        while True:
            message = await ws.recv()
            data = json.loads(message)
            print(f"Webhook received: {data}")

if __name__ == "__main__":
    asyncio.run(listen())
```

### Rust

```rust
use futures_util::{SinkExt, StreamExt};
use serde_json::json;
use tokio_tungstenite::{connect_async, tungstenite::Message};

#[tokio::main]
async fn main() {
    let url = "ws://localhost:8080/ws?topics=stripe-payments";
    
    let (mut ws, _) = connect_async(url).await.expect("Failed to connect");
    println!("Connected");

    // Authenticate
    let auth = json!({"type": "auth", "token": "my-backend-1737012345678"});
    ws.send(Message::Text(auth.to_string())).await.unwrap();

    // Wait for confirmation
    if let Some(Ok(Message::Text(resp))) = ws.next().await {
        println!("Auth: {}", resp);
    }

    // Listen for webhooks
    println!("Listening for webhooks...");
    while let Some(Ok(msg)) = ws.next().await {
        if let Message::Text(text) = msg {
            println!("Webhook received: {}", text);
        }
    }
}
```

### Node.js

```javascript
const WebSocket = require('ws');

const ws = new WebSocket('ws://localhost:8080/ws?topics=stripe-payments');

ws.on('open', () => {
    // Authenticate
    ws.send(JSON.stringify({
        type: 'auth',
        token: 'my-backend-1737012345678'
    }));
});

ws.on('message', (data) => {
    const msg = JSON.parse(data);
    
    if (msg.type === 'auth_ok') {
        console.log(`Authenticated as: ${msg.user}`);
        console.log('Listening for webhooks...');
    } else {
        console.log('Webhook received:', msg);
    }
});

ws.on('close', () => console.log('Disconnected'));
ws.on('error', (err) => console.error('Error:', err));
```

---

## CLI Commands

The CLI is for **administration only** (not for publishing/subscribing).

```bash
# Check service status
./hooklet-cli status

# Webhook management
./hooklet-cli webhook create <name>
./hooklet-cli webhook list
./hooklet-cli webhook delete <id>

# User management  
./hooklet-cli user create <name> [--subscriptions=topic1,topic2]
./hooklet-cli user list
```

### Remote Administration

By default, the CLI connects via Unix socket (local only). For remote admin:

```bash
# Set admin token on service
export HOOKLET_ADMIN_TOKEN=secret123

# Use from remote machine
./hooklet-cli --host=your-server.com --admin-token=secret123 webhook list
```

---

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP server port |
| `RABBITMQ_URL` | `amqp://guest:guest@localhost:5672/` | RabbitMQ connection |
| `HOOKLET_DB_PATH` | `./hooklet.db` | SQLite database path |
| `HOOKLET_SOCKET` | `./hooklet.sock` | Unix socket for local admin |
| `HOOKLET_ADMIN_TOKEN` | *(none)* | Required for remote administration |
| `HOOKLET_MESSAGE_TTL` | `300000` | Message TTL in queue (ms) |
| `HOOKLET_QUEUE_EXPIRY` | `3600000` | Unused queue expiry (ms) |

---

## Security Model

1. **Strict Topic Registration**: Webhooks must be created via admin before use. Unknown topics return `404`.

2. **Hashed URLs**: Webhook URLs use SHA256 of the topic name (`/webhook/<hash>`), preventing enumeration.

3. **Token Authentication**: WebSocket clients must authenticate via message (not URL) to prevent token leakage in logs.

4. **Subscription Permissions**: Users can only subscribe to topics explicitly granted to them (or `*` for all).

5. **Dual-Listener Architecture**: 
   - **TCP (public)**: Webhook ingestion + WebSocket streaming
   - **Unix Socket (local)**: Admin operations with implicit trust

---

## Architecture

```mermaid
flowchart LR
    subgraph Internet
        Stripe[Stripe / GitHub / etc.]
    end

    subgraph VPS["Public VPS"]
        Hooklet[Hooklet Service]
        RabbitMQ[(RabbitMQ)]
        SQLite[(SQLite)]
    end

    subgraph Private["Private Network"]
        App1[Your App 1]
        App2[Your App 2]
    end

    Stripe -->|POST /webhook/hash| Hooklet
    Hooklet <--> RabbitMQ
    Hooklet <--> SQLite
    Hooklet <-.->|WebSocket| App1
    Hooklet <-.->|WebSocket| App2
```

### Authentication Flow

```mermaid
sequenceDiagram
    participant Client
    participant Hooklet
    participant DB as SQLite

    Client->>Hooklet: Connect WS /ws?topics=stripe
    Hooklet-->>Client: Connection accepted
    Client->>Hooklet: {"type":"auth","token":"..."}
    Hooklet->>DB: Validate token (SHA256)
    DB-->>Hooklet: User + permissions
    alt Token valid & authorized
        Hooklet-->>Client: {"type":"auth_ok","user":"..."}
        loop Messages
            Hooklet-->>Client: Webhook payload
        end
    else Invalid or unauthorized
        Hooklet-->>Client: Close connection
    end
```

### Webhook Ingestion Flow

```mermaid
sequenceDiagram
    participant External as External Service
    participant Hooklet
    participant DB as SQLite
    participant MQ as RabbitMQ

    External->>Hooklet: POST /webhook/<hash>
    Hooklet->>DB: Lookup webhook by hash
    alt Webhook exists
        DB-->>Hooklet: Webhook found
        Hooklet->>MQ: Publish to exchange
        MQ-->>Hooklet: ACK
        Hooklet-->>External: 202 Accepted
    else Unknown hash
        DB-->>Hooklet: Not found
        Hooklet-->>External: 404 Not Found
    end
```

---

## License

MIT
