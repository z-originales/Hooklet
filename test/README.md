# Hooklet E2E Tests

End-to-end tests for the Hooklet service. These tests validate the product as a whole: they build and run the real binaries, drive real infrastructure, and assert real observable behavior. No internal packages are imported except `hooklet/internal/api` for shared constants.

## Architecture

### Black-box approach

Tests operate exclusively through the three external interfaces that real operators and clients use:

- **HTTP** — webhook publishing (`POST /webhook/{hash}`) and status (`GET /api/status`)
- **WebSocket** — consumer connections (`GET /ws?topics=...`) with token auth
- **CLI binary** — admin operations (`webhook create`, `consumer create`, etc.) via TCP or Unix socket

### Harness lifecycle

Each test calls `harness.New(t)` which does the following, in order:

1. Builds the `service` and `cli` binaries once per `go test` invocation (cached via `sync.Once`)
2. Picks three free TCP ports (service HTTP, RabbitMQ AMQP, RabbitMQ management)
3. Creates an isolated temp directory for the DB file and socket path
4. Starts a dedicated RabbitMQ container via `docker compose` using `test/docker-compose.yml`
5. Polls the container health status via `--wait`
6. Starts the `service` binary with isolated env vars pointing at the temp DB and the dedicated RabbitMQ port
7. Polls `GET /api/status` until it returns `200 OK`
8. Registers a `t.Cleanup` that kills the service, tears down the Docker project, and removes the temp dir

Every test runs against its own isolated service instance, database, socket file, and RabbitMQ container. Tests share nothing.

### Admin transports

The harness exposes three CLI helpers, each using a different transport:

| Helper | Transport | Auth |
|---|---|---|
| `h.CLI(args...)` | TCP (`HOOKLET_HOST=127.0.0.1`) | Bearer token |
| `h.CLISocket(args...)` | Unix socket | None — implicitly trusted |
| `h.CLITCPNoToken(args...)` | TCP | None — used to assert rejection |
| `h.CLITCPWrongToken(args...)` | TCP | Wrong token — used to assert rejection |

---

## Directory layout

```
test/
├── docker-compose.yml              RabbitMQ service definition
└── e2e/
    ├── internal/
    │   └── harness/
    │       └── harness.go          Lifecycle, CLI/HTTP/WS helpers, assertion helpers
    ├── smoke/                      Fast tests — run on every PR (seconds)
    │   ├── admin_test.go           Admin transports + /api/status
    │   ├── webhook_test.go         Strict registration, 503, producer auth
    │   └── ws_test.go              Happy path delivery + WS auth rejections (quick)
    └── heavy/                      Slower tests — run nightly / pre-release
        ├── retention_test.go       Offline queue retention + single active connection
        ├── ws_auth_test.go         Exhaustive WS auth negative cases (includes timeout)
        ├── tokens_test.go          Webhook set-token/clear-token + consumer regen-token
        └── wildcard_test.go        Wildcard subscription authorization matrix
```

---

## Prerequisites

- Go (any version that builds the module)
- Docker with the Compose plugin

No local RabbitMQ installation is needed. The suite manages its own containers.

---

## How to run

```bash
# Smoke suite only (fast — suitable for every PR)
go test -v ./test/e2e/smoke -count=1

# Heavy suite (slower — nightly or pre-release)
go test -v ./test/e2e/heavy -count=1 -timeout 10m

# Single test
go test -v ./test/e2e/smoke -run TestWebhookDeliveryHappyPath -count=1

# Full e2e (both suites)
go test -v ./test/e2e/... -count=1 -timeout 15m
```

`-count=1` is required to bypass Go's test result cache.

---

## Smoke tests (`test/e2e/smoke`)

Fast tests that lock in the core product contracts. Every test here must pass before a PR can merge.

### `admin_test.go`

#### `TestStatusEndpoint`
Verifies that `/api/status` returns `200 OK` with `status: ok` and `rabbitmq: connected`.

#### `TestAdminTransports`
Verifies all admin access paths in a single harness instance:

| Sub-test | What is verified |
|---|---|
| `socket_create_and_list` | CLI via Unix socket (no token) creates and lists a webhook |
| `tcp_with_token` | CLI via TCP with valid token creates webhook and consumer |
| `tcp_no_token_rejected` | CLI via TCP without token exits non-zero |
| `tcp_wrong_token_rejected` | CLI via TCP with wrong token exits non-zero |
| `direct_http_no_header_rejected` | Raw `GET /admin/webhooks` without `Authorization` → `401` |

---

### `webhook_test.go`

#### `TestWebhookStrictRegistration`
Publishing to an unknown hash returns `404 Not Found`. Locks the "secure by default" rule.

#### `TestWebhookWithoutConsumerReturns503`
A registered webhook with no active WebSocket consumer returns `503 Service Unavailable`.

#### `TestProtectedWebhookProducerAuth`
Webhooks created with `--with-token` enforce producer authentication:

| Step | Expected |
|---|---|
| POST without token | `401` |
| POST with wrong token | `401` |
| POST with correct token | `202`, message delivered to WS |

---

### `ws_test.go`

#### `TestWebhookDeliveryHappyPath`
Full end-to-end delivery: `webhook create` → `consumer create` → WS connect → auth → `POST /webhook` → WebSocket receives matching payload.

#### `TestWebSocketAuthRejections`
Quick negative auth cases:

| Sub-test | What is verified |
|---|---|
| `invalid_token` | Unknown token → `auth_failed` |
| `malformed_auth_message` | Missing `type` field → `auth_failed` |

---

## Heavy tests (`test/e2e/heavy`)

Correct and important tests that take longer to run (auth timeouts, reconnection delays, multiple harness instances). Intended for nightly CI or pre-release validation.

### `retention_test.go`

#### `TestOfflineQueueRetention`
Consumer disconnects → message published offline → consumer reconnects → retained message delivered.

#### `TestSingleActiveConnectionPerConsumer`
Reconnecting with the same consumer token kicks the previous connection with a `{"type":"kicked"}` message.

---

### `ws_auth_test.go`

#### `TestWSAuthNegativeCases`
Exhaustive matrix of WebSocket auth failures:

| Sub-test | Scenario |
|---|---|
| `missing_type` | JSON body without `type` field |
| `empty_token` | `type: auth` but `token: ""` |
| `wrong_type_value` | `type: subscribe` instead of `auth` |
| `unknown_token` | Valid structure, token not in DB |
| `auth_timeout` | No auth message sent — server closes after timeout (~10s) |

---

### `tokens_test.go`

#### `TestWebhookTokenLifecycle`
`webhook set-token` enables producer auth; `webhook clear-token` removes it. Verifies the before/during/after states with real HTTP calls.

#### `TestConsumerTokenRegeneration`
`consumer regen-token` invalidates the old token and generates a new one. Old token → WS auth rejected; new token → auth OK.

---

### `wildcard_test.go`

#### `TestWildcardSubscriptionRules`
Consumer subscribed to `orders.*`. Table-driven authorization matrix:

| Requested topic | Expected |
|---|---|
| `orders.created` | `auth_ok` (single level match) |
| `orders.eu.created` | `auth_failed` (two levels, no match) |
| `orders.*` | `auth_ok` (exact subscription string) |
| `orders.**` | `auth_failed` (not subscribed to this pattern) |

#### `TestGlobalWildcardSubscriber`
Consumer subscribed to `**` is authorized for any topic including multi-topic connections.
