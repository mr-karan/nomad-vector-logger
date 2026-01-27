<a href="https://zerodha.tech"><img src="https://zerodha.tech/static/images/github-badge.svg" align="right" /></a>

# nomad-vector-logger

Collect logs from [Nomad](https://www.nomadproject.io/) allocations with automatic metadata enrichment using [Vector](https://vector.dev/).

## The Problem

Nomad stores task logs in a flat directory structure with **zero context**:

```
/opt/nomad/data/alloc/
├── 64a2f9fd-e003-0bb3-b5cd-838125283a06/
│   └── alloc/logs/
│       ├── proxy.stdout.0      ← Which job? Which namespace? 🤷
│       └── proxy.stderr.0
├── a1b2c3d4-xxxx-.../
│   └── ...
```

For `raw_exec` and `exec` drivers, there's no built-in way to identify which job/task a log belongs to. This project solves that.

## Quick Start

### 1. Create the data directory

```bash
mkdir -p /var/lib/vector
```

### 2. Deploy as Nomad system job

```hcl
job "vector" {
  type = "system"
  
  group "vector" {
    task "init" {
      driver = "raw_exec"
      lifecycle {
        hook    = "prestart"
        sidecar = false
      }
      config {
        command = "/bin/mkdir"
        args    = ["-p", "/var/lib/vector"]
      }
    }

    task "vector" {
      driver = "raw_exec"
      
      artifact {
        source = "https://github.com/vectordotdev/vector/releases/download/v0.52.0/vector-0.52.0-x86_64-unknown-linux-gnu.tar.gz"
      }
      
      config {
        command = "${NOMAD_TASK_DIR}/vector-x86_64-unknown-linux-gnu/bin/vector"
        args    = ["--config", "${NOMAD_TASK_DIR}/vector.yaml"]
      }
      
      template {
        data        = file("vector.yaml")
        destination = "local/vector.yaml"
      }
      
      template {
        data        = <<EOH
NOMAD_ADDR=http://{{ env "NOMAD_IP_metrics" }}:4646
NOMAD_NODE_ID={{ env "node.unique.id" }}
NOMAD_TOKEN={{ with nomadVar "nomad/jobs/vector" }}{{ .NOMAD_TOKEN }}{{ end }}
EOH
        destination = "secrets/env.env"
        env         = true
      }
    }
  }
}
```

### 3. Add a sink to vector.yaml

```yaml
sinks:
  clickhouse:
    type: clickhouse
    inputs: ["nomad_logs_formatted"]
    endpoint: "http://clickhouse:8123"
    database: "logs"
    table: "nomad_apps"
```

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                                  Node                                       │
│                                                                             │
│                            ┌─────────────────────────────────────────────┐  │
│                            │                 Vector                      │  │
│                            │                                             │  │
│  ┌─────────────────┐       │  ┌─────────────┐     ┌───────────────────┐  │  │
│  │   Nomad API     │◀──────│──│ http_client │────▶│     transform     │  │  │
│  │ :4646/v1/node/  │ poll  │  │ (30s interval)    │ (build lookup map)│  │  │
│  └─────────────────┘       │  └─────────────┘     └─────────┬─────────┘  │  │
│                            │                                │            │  │
│                            │                                ▼            │  │
│                            │                      ┌───────────────────┐  │  │
│                            │                      │ memory enrichment │  │  │
│                            │                      │      table        │  │  │
│                            │                      │  (TTL: 120s)      │  │  │
│                            │                      └─────────┬─────────┘  │  │
│                            │                                │            │  │
│                            │  ┌─────────────┐     ┌─────────▼─────────┐  │  │
│  /opt/nomad/data/          │  │ file source │────▶│      remap        │──│──▶ Sink
│  alloc/*/logs/* ───────────│─▶│ (glob pattern)    │ (enrich + lookup) │  │  │
│                            │  └─────────────┘     └───────────────────┘  │  │
│                            │                                             │  │
│                            └─────────────────────────────────────────────┘  │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `NOMAD_NODE_ID` | Yes | - | Node UUID (use `${node.unique.id}` in Nomad) |
| `NOMAD_ADDR` | No | `http://localhost:4646` | Nomad API address |
| `NOMAD_TOKEN` | No | - | ACL token with `node:read`, `namespace:read-job` |
| `NOMAD_DATA_DIR` | No | `/opt/nomad/data/alloc` | Nomad allocation data directory |
| `VECTOR_DATA_DIR` | No | `/var/lib/vector` | Vector checkpoint directory (must be persistent!) |

## Sample Output

**Input** (raw log line):
```
192.168.29.76 - - [03/Sep/2022:17:30:36 +0000] "GET / HTTP/1.1" 200
```

**Output** (enriched JSON from `nomad_log_enrich`):
```json
{
  "file": "/opt/nomad/data/alloc/64a2f9fd-e003-0bb3-b5cd-838125283a06/alloc/logs/proxy.stdout.0",
  "message": "192.168.29.76 - - [03/Sep/2022:17:30:36 +0000] \"GET / HTTP/1.1\" 200",
  "nomad": {
    "alloc_id": "64a2f9fd-e003-0bb3-b5cd-838125283a06",
    "namespace": "default",
    "node_name": "worker-1",
    "job_name": "nginx",
    "group_name": "web",
    "task_name": "proxy",
    "job_type": "service"
  },
  "timestamp": "2022-09-03T17:30:42.569Z"
}
```

## Key Implementation Details

### Memory Enrichment Table Data Model

The memory enrichment table uses a **key-value model** where:
- The **field name** in the output object becomes the lookup key
- The **field value** becomes the stored data

```yaml
# Builder outputs: { "alloc-id-here": { job: "...", namespace: "..." } }
. = set!(value: {}, path: [alloc_id], data: metadata)

# Lookup with: { "key": alloc_id }
record = get_enrichment_table_record("table", { "key": alloc_id })
# Returns: { "key": "alloc-id", "value": { job: "...", ... }, "ttl": 120 }
```

### Fingerprint Strategy

Use `device_and_inode` instead of the default `checksum`:

```yaml
fingerprint:
  strategy: device_and_inode
```

The default `checksum` strategy fails on small/empty log files because it needs minimum bytes to compute a fingerprint.

### Persistent Checkpoints

Vector checkpoints track file read positions. Without persistence, Vector re-reads all logs on restart.

```yaml
data_dir: "/var/lib/vector"  # Must survive restarts!
```

**Important**: If using Nomad's `${NOMAD_ALLOC_DIR}`, checkpoints are lost on each deployment since alloc IDs change.

### Startup Race Condition

Logs may arrive before the first Nomad API poll completes (~30s). These logs can't be enriched. Options:

1. **Drop unenriched logs** (recommended):
   ```yaml
   if .nomad.namespace == null || .nomad.namespace == "" {
     abort
   }
   ```

2. **Reduce poll interval** (increases API load):
   ```yaml
   scrape_interval_secs: 5
   ```

## Requirements

- **Vector 0.49+** (memory enrichment table support)
- **Nomad ACL policy** (if ACLs enabled):

```hcl
namespace "*" {
  policy = "read"
}
node {
  policy = "read"
}
```

## License

[LICENSE](./LICENSE)
