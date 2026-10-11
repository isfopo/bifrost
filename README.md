# Bifrost

Bifrost is a small, self-hostable webhook bridge: services can't call each
other's APIs directly, so Bifrost receives a webhook from one service
(`from`), optionally filters it, reshapes it with a declarative mapping, and
delivers it to another (`to`). The event machinery is generic — all
specificity lives in `config.yaml`.

Built for homelab wiring like **Plane → CalDAV** or **Donetick → CalDAV** without
running a full workflow engine.

## Quickstart

### Local

```sh
go build -o bifrost ./cmd/bifrost
./bifrost                       # CONFIG defaults to ./config.yaml
```

### Docker

```sh
cp .env.example .env            # fill in the secrets your config.yaml references
docker compose up -d
```

Runs the published image (`ghcr.io/isfopo/bifrost`, pin a version tag in
`docker-compose.yml` for unattended hosts). For building your own fork:
replace the `image:` line and use `docker compose up -d --build` — the
`Dockerfile` is the same one CI publishes from.

The sample `config.yaml` ships with a **log** target so you can verify plumbing
before pointing anything at a real service. Open the log target by POSTing to
the flow's endpoint (see below).

## Configuration

Schema overview (`config.yaml`; secrets come from env, never the file):

```yaml
addr: ":29618"

services:
  plane:                        # reusable service definition
    from:                       # how inbound webhooks from it are verified
      auth:
        type: hmac              # none | header | hmac
        header: X-Plane-Signature
        secret_env: PLANE_WEBHOOK_SECRET
        prefix: "sha256="
        encoding: hex           # hex (default) | base64

  caldav:
    to:                         # how Bifrost delivers to it
      kind: caldav              # log | http | caldav
      base_url: https://dav.internal/dav/james/calendars/projects/
      auth:
        type: basic             # none | bearer | basic
        username_env: CALDAV_USER
        password_env: CALDAV_PASS
      timeout: 10s
      attempts: 3               # retries on 5xx/network errors, exp. backoff

flows:
  plane-to-caldav:
    from: {service: plane}
    to:   {service: caldav}     # optional endpoint: <path or JSONata expr>
    when: $.event = "issue.created"    # optional JSONata filter
    map: |
      {
        "uid": $.issue.id,
        "summary": $.issue.title,
        "description": $.issue.description,
        "due": $.issue.target_date
      }
```

- `when` — a JSONata boolean expression; a flow without one matches everything.
- `map` — a JSONata expression that builds the outgoing body from the event.
- `endpoint` (on the `to` ref) — a static path, or a JSONata expression
  (anything containing `$`) evaluated against the *mapped* output.

The default listen port is **29618** (A1Z26 of **BIFR** — the first four
letters of BIFROST: B=2, I=9, F=6, R=18). It's outside every default
ephemeral port range (Linux 32768–60999, Windows 49152–65535), outside
Kubernetes NodePort (30000–32767), and unassigned by IANA — so it won't
collide with OS transient connections or anything you already host.
Override it with the config `addr` or the `ADDR` env var.

### JSONata gotchas

- The input document is `$`. Paths are `$.issue.title`.
- **`$event` is wrong** — that reads *a variable* named `event` (undefined).
  Use `$.event` for top-level fields. Test expressions at
  [try.jsonata.org](https://try.jsonata.org) or with the `/test` endpoint.
- `map` must produce a JSON object or array. For `kind: caldav` the object
  needs `uid`, and may include `summary`, `description`, `due` (`YYYY-MM-DD`
  or RFC3339), `status`, `priority` (1–9), `percent` (0–100).
- Config (and its expressions) is compiled once at startup; a syntax error
  fails fast instead of half-serving.

## Endpoints

| Route | Purpose |
|---|---|
| `POST /hooks/{flow}` | Inbound webhook for a flow. Auth per the flow's `from` service; returns 202 for non-matching events, 200 on delivery. |
| `POST /hooks/{flow}/test` | Dry run: returns `{matched, payload}` — the authoring loop. Bypasses auth by design; add `?execute=true` to actually deliver. |
| `GET /healthz` | Health (used by the Docker healthcheck). |
| `GET /` | Status + flow list. |

Example authoring loop:

```sh
curl -s localhost:29618/hooks/foo-to-calendar/test \
  -H 'Content-Type: application/json' \
  -d '{"event":"issue.created","issue":{"id":"123","title":"Build shed"}}'
```

## Targets

- **`kind: log`** — writes the would-be payload to stdout; zero risk while
  developing a new mapping.
- **`kind: http`** — generic request; `url` or `base_url` + `endpoint`,
  custom `headers`, `method` (default POST), bearer/basic auth. Body is the
  mapped JSON.
- **`kind: caldav`** — PUTs a VTODO object to `<base_url>/<endpoint or
  `<uid>.ics`>` with `Content-Type: text/calendar`, `If-None-Match: *`.
  A 412 (UID already exists) is treated as success, so retries/replays are
  idempotent. Works with Baikal, Nextcloud, Radicale, ...
- **`kind: smtp`** — sends the mapped payload as an email. Connection
  properties are service config (`host`, `port`, `from`, `tls: none |
  starttls | tls`, optional `to` fallback recipient, `basic` auth); the
  payload is the message: `to` (string or array), `subject`, `text` and/or
  `html`. With no body field, the whole payload is pretty-printed as the
  text body. The flow `endpoint` overrides the recipient. Header values are
  CRLF-stripped to prevent header injection.
- **`kind: mqtt`** — publishes the mapped payload as JSON to a topic.
  Service config: `broker` (`tcp://` or `ssl://`), optional `topic`
  fallback, `client_id` (default `bifrost`), `qos` (default 1), `retain`,
  anonymous or `basic` credentials, and `insecure_tls` for self-signed
  brokers. The flow `endpoint` is the topic — including JSONata-produced
  topics (e.g. `"alerts/" & $.severity`).

Adapters are deliberately dumb *codecs for a wire format*, not per-service
APIs: everything about which API you call and what objects get sent lives in
the flow mapping. If an integration can't be expressed with `http`, `smtp`
or `mqtt`, that's when a new adapter earns its place (`kind` is the
extension point — see `internal/deliver/`).

Example: high-priority Push events as email via SMTP:

```yaml
services:
  plane:
    from:
      auth: {type: hmac, header: X-Plane-Signature, secret_env: PLANE_WEBHOOK_SECRET, prefix: "sha256="}
  mail:
    to:
      kind: smtp
      host: smtp.internal
      from: plane-alerts@internal
      to: james@internal
flows:
  plane-urgent-mail:
    from: {service: plane}
    to: {service: mail}
    when: $.event = "issue.updated" and $.issue.priority.name = "Urgent"
    map: |
      {
        "subject": "Urgent: " & $.issue.title,
        "text": "Updated to " & $.issue.state.name & " by " & $.issue.assignees[0].name
      }
```

Retries: first try + `attempts-1` retries with exponential backoff (0.5s base)
on network errors and 5xx only. 4xx responses fail the delivery — that's a
config or auth problem, not a transient one.

## Security notes

- Inbound auth is per-service: HMAC-SHA256 (provider-style headers, optional
  value prefixes), static header token, or none.
- Outbound requests to non-loopback hosts must be HTTPS (loopback is allowed
  so you can test against local services).
- Secrets are only read from environment variables at startup.
- `/test` deliberately bypasses auth — keep the management surface
  (`/`, `/hooks/*/test`) off the public internet.

## Development

```sh
go test ./...     # unit tests cover auth, routing, mapping, CalDAV output
go vet ./...

# Layout (standard Go app conventions):
#   cmd/bifrost/main.go     entrypoint — thin; startup + graceful shutdown only
#   internal/config/        YAML schema, load, validation, secret resolution
#   internal/auth/          inbound webhook verification (none/header/hmac)
#   internal/flow/          JSONata compile + eval: when / map / endpoint
#   internal/deliver/       outbound adapters: log, http, caldav (VTODO)
#   internal/server/        HTTP wiring, handlers, request pipeline
#   internal/               may only be imported within this module
```

Roadmap ideas: event persistence + replay, config hot-reload, more adapters
(HMAC providers presets), multi-source fanout per flow.

## License

[MIT](LICENSE) — free for any use; the sample hostnames, service names and
credentials in `config.yaml` / `.env.example` are placeholders, nothing
identifying is tracked.
