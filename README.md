# Distributed Key-Value Store

A fault-tolerant, distributed key-value store built from scratch in Go, implementing the **Raft consensus algorithm** for leader election, log replication, and crash recovery.

## What this is

Most databases you use day-to-day run on a single machine. If that machine goes down, so does your data. Distributed databases solve this by replicating state across multiple nodes — but replication is only useful if every node agrees on what the current state is. That agreement problem is called **consensus**, and Raft is the algorithm this project uses to solve it.

This project is a complete implementation of that system: three nodes that coordinate over HTTP to elect a leader, replicate every write through a shared log, commit only once a majority acknowledges, and recover automatically when nodes crash or get partitioned from the network.

The dashboard lets you observe and interact with the cluster in real time — you can send writes, watch them propagate through the log, partition individual nodes, and see a new leader elected from the remaining majority within milliseconds.

## How it works

Every write goes through the same path regardless of which node you send it to. The receiving node forwards it to the leader (if it isn't the leader itself). The leader appends the command to its local log, then sends that entry to all followers via AppendEntries RPCs. Once a majority of nodes acknowledge the entry, the leader marks it committed and applies it to the key-value state machine. Followers apply it on their next heartbeat cycle. Reads can be served by any node from local state.

When a node stops sending heartbeats — whether because it crashed or was partitioned — the remaining nodes detect the timeout and start a new election. The first node whose election timer fires increments its term, votes for itself, and requests votes from peers. The node that collects a majority becomes the new leader and immediately starts replicating. For a 3-node cluster, this takes between 200 and 400ms.

When a partitioned node rejoins, it discovers it's behind by comparing terms with the new leader. The leader sends it all missing log entries, and the node replays them in order to catch up.

## Stack

Go 1.22 · standard library only (no external dependencies)

## Quick start

```bash
# Build and launch a 3-node cluster
./scripts/start.sh

# Open the dashboard
open web/index.html

# Stop everything
./scripts/stop.sh
```

The three nodes listen on `:8001`, `:8002`, `:8003`.
The dashboard polls all nodes every 600ms and uses SSE for instant role-change events.

## Dashboard

- **Node diagram** — shows each node as a circle; gold = leader (pulsing), teal = follower, orange = candidate, gray = partitioned
- **PARTITION / RECONNECT** — simulate a network partition on any node; watch the remaining two elect a new leader within ~400ms
- **PUT / GET / DELETE** — send commands through the cluster; the dashboard auto-routes to the leader
- **Replicated Log** — live view of every committed log entry (index, term, command)
- **Store State** — current key-value snapshot

## HTTP API

```
POST /kv/put      {"key":"foo","value":"bar"}
GET  /kv/get?key=foo
POST /kv/delete   {"key":"foo"}
GET  /kv/all

GET  /status              node status snapshot
GET  /events              SSE stream of Raft events

POST /admin/pause         simulate network partition
POST /admin/resume        reconnect node
```

## Architecture

```
cmd/kvnode/main.go          entrypoint — parses flags, starts server
internal/raft/raft.go       Raft consensus (election, replication, persistence)
internal/raft/types.go      RPC types, log entry, node status
internal/store/store.go     KV state machine (applied after commit)
internal/server/server.go   HTTP layer + SSE broadcaster
web/index.html              live cluster dashboard
```

Persistence is stored under `data/node{id}.json` — delete to reset cluster state.