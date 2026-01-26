# 🪝 Hooklet

> Lightweight webhook relay — receive webhooks on a VPS, stream them to your apps via WebSocket.

```
[Stripe/GitHub/etc] ──POST──▶ [Hooklet on VPS] ══WebSocket══▶ [Your App behind NAT]
```

No inbound ports needed. No ngrok. Just outbound WebSocket connections.

---

## ✨ Features

- **⚡ Real-Time** — Instant delivery via WebSocket
- **📡 Multi-Topic** — Subscribe to multiple webhooks on one connection  
- **🔒 Secure by Default** — Token auth, hashed URLs, strict topic registration
- **🐰 Reliable** — Built on RabbitMQ with message persistence (5 min retention)
- **🪶 Lightweight** — Single binary, SQLite storage, minimal dependencies

---

## 🚀 Quick Start

### Prerequisites

- Go 1.24+
- Docker (for RabbitMQ)

### 1. Start Infrastructure

```bash
docker-compose up -d
```

### 2. Build & Run

```bash
go build -o hooklet ./cmd/service
go build -o hooklet-cli ./cmd/cli

./hooklet
```

### 3. Create a Webhook

```bash
./hooklet-cli webhook create stripe-payments
```
```
✓ Webhook created: stripe-payments
  URL: /webhook/a1b2c3d4e5f6...
```

### 4. Create a Consumer

```bash
./hooklet-cli consumer create my-app --subscriptions=stripe-payments
```
```
✓ Consumer created: my-app
  Token: my-app-1737012345678
  ⚠️  Save this token! It won't be shown again.
```

### 5. Connect & Listen

```
Connect:  ws://localhost:8080/ws?topics=stripe-payments
Send:     {"type":"auth","token":"my-app-1737012345678"}
Receive:  {"type":"auth_ok","consumer":"my-app"}
Stream:   ... webhooks arrive here ...
```

---

## 📖 Client Examples

<details>
<summary><b>Go</b></summary>

```go
conn, _, _ := websocket.Dial(ctx, "ws://localhost:8080/ws?topics=stripe-payments", nil)
defer conn.Close(websocket.StatusNormalClosure, "")

// Auth via message (not URL — prevents log leakage)
conn.Write(ctx, websocket.MessageText, []byte(`{"type":"auth","token":"my-app-xxx"}`))

for {
    _, msg, _ := conn.Read(ctx)
    fmt.Printf("Webhook: %s\n", msg)
}
```
</details>

<details>
<summary><b>Python</b></summary>

```python
async with websockets.connect("ws://localhost:8080/ws?topics=stripe-payments") as ws:
    await ws.send(json.dumps({"type": "auth", "token": "my-app-xxx"}))
    async for message in ws:
        print(f"Webhook: {message}")
```
</details>

<details>
<summary><b>Node.js</b></summary>

```javascript
const ws = new WebSocket('ws://localhost:8080/ws?topics=stripe-payments');
ws.on('open', () => ws.send(JSON.stringify({type: 'auth', token: 'my-app-xxx'})));
ws.on('message', (data) => console.log('Webhook:', JSON.parse(data)));
```
</details>

<details>
<summary><b>Rust</b></summary>

```rust
let (mut ws, _) = connect_async("ws://localhost:8080/ws?topics=stripe-payments").await?;
ws.send(Message::Text(r#"{"type":"auth","token":"my-app-xxx"}"#.into())).await?;
while let Some(Ok(Message::Text(msg))) = ws.next().await {
    println!("Webhook: {}", msg);
}
```
</details>

---

## 🛠️ CLI Reference

```bash
# Service health
hooklet-cli status

# Webhooks
hooklet-cli webhook create <name>
hooklet-cli webhook list
hooklet-cli webhook delete <id>

# Consumers
hooklet-cli consumer create <name> [--subscriptions=topic1,topic2]
hooklet-cli consumer list
hooklet-cli consumer delete <id>
hooklet-cli consumer subscribe <id> --topic=<pattern>
hooklet-cli consumer unsubscribe <id> --topic=<pattern>
hooklet-cli consumer set-subs <id> --subscriptions=<patterns>
hooklet-cli consumer regen-token <id>
```

### Subscription Patterns

Consumers can subscribe to topics using glob patterns:

| Pattern | Matches | Example |
|---------|---------|---------|
| `orders.created` | Exact topic | `orders.created` only |
| `orders.*` | Single level | `orders.created`, `orders.updated` |
| `orders.**` | All levels | `orders.created`, `orders.eu.created` |
| `**` | Everything | All topics (admin access) |

```bash
# Subscribe to all order events
hooklet-cli consumer subscribe 1 --topic="orders.*"

# Subscribe to everything (admin)
hooklet-cli consumer subscribe 1 --topic="**"
```

### Remote Administration

```bash
# On the server
export HOOKLET_ADMIN_TOKEN=secret123

# From your machine
hooklet-cli --host=your-server.com --admin-token=secret123 webhook list
```

---

## ⚙️ Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP server port |
| `RABBITMQ_URL` | `amqp://guest:guest@localhost:5672/` | RabbitMQ connection |
| `HOOKLET_DB_PATH` | `./hooklet.db` | SQLite database path |
| `HOOKLET_SOCKET` | `./hooklet.sock` | Unix socket for local CLI |
| `HOOKLET_ADMIN_TOKEN` | — | Required for remote admin |
| `HOOKLET_MESSAGE_TTL` | `300000` | Message retention in queue (ms, default 5 min) |
| `HOOKLET_QUEUE_EXPIRY` | `3600000` | Unused queue lifetime (ms, default 1 hour) |

---

## 🏗️ Architecture

```mermaid
flowchart LR
    subgraph Internet
        Stripe[Stripe / GitHub / etc.]
    end

    subgraph VPS["☁️ Public VPS"]
        Hooklet[Hooklet Service]
        RabbitMQ[(RabbitMQ)]
        SQLite[(SQLite)]
    end

    subgraph Private["🏠 Private Network"]
        App1[Your App 1]
        App2[Your App 2]
    end

    Stripe -->|POST /webhook/hash| Hooklet
    Hooklet <--> RabbitMQ
    Hooklet <--> SQLite
    Hooklet <-.->|WebSocket| App1
    Hooklet <-.->|WebSocket| App2
```

---

## 🔒 Security

- **Hashed webhook URLs** — `/webhook/a1b2c3...` makes topic enumeration more difficult
- **Token-based auth** — Consumers authenticate via message, not URL (no log leakage)
- **Stored hashed** — Tokens stored as SHA256, never in plaintext
- **Strict registration** — Only pre-registered webhooks accept data (404 otherwise)
- **Unix socket admin** — Local CLI has implicit trust, remote requires token