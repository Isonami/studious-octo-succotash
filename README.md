# Studious Octo Succotash

Web UI for downloading remote directories with rsync.

## Sync workers and Redis

Redis is required. Set `REDIS_URL` (default `redis://localhost:6379/0`;
`rediss://` enables TLS) and optionally `REDIS_PREFIX` (default `studious:`).
All web replicas and workers managing the same data must use the same Redis
URL and prefix. Use a separate prefix for independent installations. Configure
Redis persistence and a no-eviction policy for reliable coordination.

Sync reservations, progress and durable stop requests live in Redis. Workers
publish progress and poll for stop requests every second. Cancellation stops
the rsync process group, escalating to SIGKILL after five seconds. Web server
shutdown does not cancel workers. Redis failures cause workers to stop rather
than continue without coordination. Active worker leases expire after two
minutes without updates; queued launches expire after ten minutes. Finished
records are retained for up to 24 hours and pruned during listing/reservation;
`/api/syncs` continues to return only active downloads.

Outside Kubernetes the server starts a detached copy of its own executable,
forwarding configuration flags and environment. It does not require a shell or
keep an HTTP request open. Detached worker output is discarded; state and
failure details are stored in Redis. `WORKER_EXECUTABLE` can override the binary
path (the same path must exist in worker containers). Container runtimes may
kill all processes when a container exits; use Kubernetes Jobs for workers
that must survive replacement of the web container.

### Optional MQTT notifications

Set `MQTT_BROKER` to enable lifecycle notifications, for example
`tcp://mqtt:1883` or `ssl://mqtt.example.com:8883` (verified TLS). WebSocket
URLs (`ws://` and `wss://`) are also supported. Optional settings:

- `MQTT_TOPIC`: publish topic (default `studious/sync`; no wildcards).
- `MQTT_USERNAME` and `MQTT_PASSWORD`: broker authentication.

Workers publish JSON events to the configured topic:

```json
{"event":"start","id":"sync-uuid","path":"/shows/example","state":"running","timestamp":"2026-01-01T12:00:00Z"}
```

Events are `start`, `complete`, `error`, and `cancelled`. Error events include
an `error` string. A stop request is reported as `cancelled`, not as an error.
The web process also publishes an error if it cannot launch a worker. Start
means the worker is about to invoke rsync; complete is sent only after rsync
succeeds and final state is saved to Redis.

Messages use QoS 1 without retention. Consumers should tolerate duplicates
and correlate events by `id`. Notifications are best effort: connection and
publish operations have five-second timeouts, and MQTT failures are logged
without changing the sync result. Events are not queued for later replay, and
abruptly killed workers cannot publish a terminal event. Kubernetes workers
inherit MQTT settings from the same configuration as the web container and
need network access to the broker. Leave `MQTT_BROKER` unset to disable MQTT.

### Kubernetes

When `KUBERNETES_SERVICE_HOST` is set, each sync launches a Job rather than a
local process. Kubernetes API errors are surfaced, not silently downgraded to
local execution. The application reads its own Pod using the in-cluster service
account. Set `POD_NAME` via the downward API if `HOSTNAME` is not the Pod name.
Set `KUBERNETES_CONTAINER` for a Pod with multiple containers to select the
application container.

Jobs reuse the application container's image, environment, mounts, resources,
security configuration and relevant Pod configuration. Health probes and web
ports are removed; the worker executes the same binary with `--sync_worker`.
Jobs do not retry automatically and are cleaned up after completion.

**Data and SSH configuration must be accessible to the worker.** Use shared
persistent storage for `DATA_PATH`, and mount SSH keys / known_hosts through
Secrets or shared volumes. `emptyDir` and container-local files are not shared
between the web Pod and Job. Ensure storage access modes and scheduling permit
the web Pod and Jobs to mount the volumes simultaneously. Job Pods need access
to Redis and the SSH destination.

Grant the web Pod's service account namespace-scoped permissions (bind this
Role with a RoleBinding):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: sync-job-launcher
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["create"]
```

Existing remote, SSH, data path and optional OIDC configuration remains
unchanged. Configuration uses environment variables or flags, e.g.
`--redis_url=redis://redis:6379/0`.
