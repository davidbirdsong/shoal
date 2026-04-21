# shoal

Gossip-empowered service bridge for ECS tasks and HAProxy.

> A **shoal** is a loosely coordinated group of fish — each individual moves on its
> own, aware of its neighbors through local signals, with no central fish in charge.
> That's exactly what this is: ephemeral tasks that find each other through gossip,
> self-register without a registry, and scatter cleanly when one goes down. The
> HAProxy sidecar is just the surface they swim near.


Shoal connects ephemeral ECS tasks to a HAProxy singleton using
[hashicorp/serf](https://github.com/hashicorp/serf) gossip — no service registry,
no polling, no external coordination service. Tasks self-register by issuing a
query to the sidecar on startup. Failure detection is handled by memberlist; dead
tasks are removed from HAProxy within seconds.

---

## Architecture

```
                    Elastic IP
                        │
              ┌─────────▼──────────┐
              │  HAProxy container  │
              │  ┌───────────────┐  │
              │  │ shoal sidecar │  │  ← gossip member + HAProxy socket writer
              │  └───────────────┘  │
              └────────────────────┘
                        │  gossip cluster
          ┌─────────────┼─────────────┐
          │             │             │
   ┌──────▼──────┐ ┌────▼──────┐ ┌───▼───────┐
   │ shoal task  │ │ shoal task│ │ shoal task│
   │  ┌────────┐ │ │ ┌───────┐ │ │ ┌───────┐ │
   │  │ worker │ │ │ │worker │ │ │ │worker │ │
   │  └────────┘ │ │ └───────┘ │ │ └───────┘ │
   └─────────────┘ └───────────┘ └───────────┘
```

**Task startup sequence:**

1. Bind an ephemeral TCP port (`SHOAL_PORT`, `SHOAL_LISTENER_FD=3`)
2. Fork the worker subprocess, passing the listener fd as fd 3
3. Wait for worker to be ready
4. Join the gossip cluster via the sidecar's elastic IP
5. Issue an `"announce"` query to the sidecar carrying `addr:port`
6. Sidecar adds the backend to HAProxy via the runtime UNIX socket — no reload

**Task shutdown sequence:**

1. Receive `SIGTERM`
2. Issue a `"depart"` query to the sidecar
3. Sidecar removes the backend from HAProxy
4. Task drains and exits

If a task dies without departing, memberlist failure detection fires within seconds
and the sidecar removes the backend via the `EventMemberFailed` handler.

---

## Subcommands

### `shoal sidecar`

Runs on the HAProxy container. Joins the gossip cluster, listens for task
registrations, and drives the HAProxy runtime socket.

```
Flags:
  --join strings                 seed addresses to join (required)
  --haproxy-socket string        HAProxy runtime UNIX socket path (default "/var/run/haproxy/admin.sock")
  --haproxy-backend string       HAProxy backend pool name (default "shoal")
  --snapshot-dir string          serf snapshot directory (default "./serf-snapshots")
  --solicit-interval duration    interval between solicit queries to tasks (default 30s)
```

### `shoal task`

Runs as the parent process of each backend ECS task. Binds an ephemeral port,
forks the worker, and manages cluster membership.

```
Flags:
  --join string              sidecar address to join (required)
  --snapshot-dir string      serf snapshot directory (default "./serf-snapshots")
  --drain-timeout duration   max time to drain before hard exit (default 30s)
  --worker stringArray       worker command and arguments
```

**Example:**

```sh
shoal task --join 10.0.0.1:7946 --worker python3 server.py
```

The worker receives:
- `SHOAL_PORT` — the bound port number
- `SHOAL_ADDR` — the bound address
- `SHOAL_LISTENER_FD=3` — an already-bound listener fd (call `accept()` directly)

---

## Query protocol

Shoal uses serf queries as a lightweight RPC transport. The query `Name` is the
method discriminator; payloads are protobuf-encoded.

| Query       | Issuer  | Request            | Response           |
|-------------|---------|--------------------|--------------------|
| `announce`  | task    | `AnnounceRequest`  | `AnnounceResponse` |
| `solicit`   | sidecar | _(empty)_          | `AnnounceRequest`  |
| `depart`    | task    | `DepartRequest`    | `DepartResponse`   |

Proto sources: [`proto/shoal.proto`](proto/shoal.proto)

---

## Building

```sh
# Build binary
go build -o shoal ./cmd/

# Build Docker image (linux/arm64)
make build

# Push to ECR
make push

# Regenerate protobuf Go code
# requires: protoc, protoc-gen-go
make proto
```

---

## Testing

```sh
go test ./...
```

---

## Package layout

| Package | Purpose |
|---|---|
| `cmd/` | Cobra CLI — `shoal sidecar`, `shoal task` |
| `pkg/node` | Serf node setup and event dispatch loop |
| `pkg/cluster` | Shared constants (tag keys, roles, query names) |
| `pkg/proto` | Generated protobuf types and marshal helpers |
| `proto/` | Protobuf source definitions |
