# StarlinkBridge

Read-only Starlink monitoring in Go, with local gRPC and consumer-cloud gRPC-Web
data sources, Prometheus metrics, and Home Assistant MQTT discovery. This is an
independent project and is not affiliated with SpaceX or Starlink.

The useful dish-specific readings include power consumption, outage causes and
durations, obstruction data, heating/thermal alerts, physical alignment, firmware
state and uptime. Traffic, latency and loss are also available. Both sources use
the same decoder and stable dish identity, so source changes do not create new HA
devices. One bridge instance monitors one dish; configure separate instances and
MQTT prefixes/client IDs for multiple dishes.

## Quick start

```powershell
cd bridge
go test ./...
$env:STARLINK_MODE = 'local'
go run . fetch
go run . serve
```

Local mode requires network access to `192.168.100.1:9200`. Cloud mode needs only
internet access and an authenticated Starlink browser session. Auto mode tries
local first, falls back to cloud after a local failure, and tries local again on
the next poll. A configured cloud session is maintained even while local works.

For cloud mode, save the **Cookie request-header value** from an authenticated
Starlink account request into `secrets/starlink.cookie` (one line, no quotes):

```powershell
$env:STARLINK_MODE = 'cloud'
$env:STARLINK_COOKIE_FILE = '../secrets/starlink.cookie'
$env:STARLINK_COOKIE_STATE_FILE = '../data/cookies.json'
go run . fetch
go run . serve
```

Browser developer tools: sign in to `https://www.starlink.com/account/home`, open
Network, reload, select `api.starlink.com/auth-rp/auth/user` or `service-lines`, and
copy its Request Headers > Cookie value. A separate incognito session is suitable.
Do not log out of that session: explicit logout can invalidate the bridge session.
Closing the browser is different from signing out, but server session policy still
controls lifetime. Never put cookies in an image, Git, command-line arguments, or
chat. `secrets/`, `data/` and `.env` are ignored by Git and excluded from builds.

HTTP endpoints on port 9091:

- `/metrics`: cached Prometheus measurements; scraping never polls Starlink.
- `/api/v1/status`: normalized snapshot and collector/session health.
- `/healthz`: process liveness (healthy even when Starlink needs authentication).
- `/readyz`: HTTP 200 only while a recent successful snapshot is available.

## Session rotation

The cookie header is a bootstrap, not the ongoing session database. The client
loads the persisted cookie jar, checks the bootstrap file for changes, and imports
new content without requiring a restart. An unchanged old bootstrap cannot replace
newer rotated cookies after restart. Every Starlink `Set-Cookie` is applied with
domain/path/expiry/deletion semantics and saved atomically with private file
permissions. Session cookies are persisted too. The writable data directory must
remain on a persistent NAS volume and be accessible to the container user.
A copied Cookie header has no scope metadata: bootstrap cookies start as Secure
cookies for `.starlink.com` at `/`; server updates supply their actual scope.

Starlink renews its chunked access cookie on `api.starlink.com`, while dish RPCs
use the account site's origin. The client explicitly supplies the renewed
`Starlink.Com.Access.V1` cookie family to that fixed RPC endpoint and suppresses
older bootstrap copies. The marker and chunks stay together; unrelated host-only
cookies retain their normal scope. Existing saved sessions recover this selection
after a bridge update or restart.

The client checks/renews through `auth-rp/auth/user` at a configurable interval
(30 minutes by default) and performs one bounded refresh/retry after an HTTP 401.
Repeated failures back off. A changed bootstrap file is picked up promptly. A
separate HA authentication-required diagnostic and Prometheus health metric report
expired/revoked sessions or denied access; fresh cookies may not fix denied access.
Cookies cannot be renewed forever: Starlink can
revoke them or demand a new login/MFA. Replace `starlink.cookie` with a new full
Cookie header when that happens. The bootstrap remains read-only to the container;
do not manually replace the generated `cookies.json` with a browser header.

## Configuration

| Environment variable | Default | Purpose |
|---|---|---|
| `STARLINK_MODE` | `auto` | `local`, `cloud`, or local-first `auto` |
| `STARLINK_LOCAL_ADDRESS` | `192.168.100.1:9200` | Local gRPC endpoint |
| `STARLINK_DEVICE_ID` | empty | Cloud dish target; required if account has multiple dishes |
| `STARLINK_CLOUD_WEB_HOST` | `starlink.com` | `starlink.com` or `www.starlink.com` |
| `STARLINK_COOKIE_FILE` | `/app/config/starlink.cookie` | Read-only bootstrap header |
| `STARLINK_COOKIE_STATE_FILE` | `/app/data/cookies.json` | Private rotating session state |
| `STARLINK_COOKIE_REFRESH_INTERVAL` | `30m` | Periodic session check, between 1m and 12h |
| `STARLINK_POLL_INTERVAL` | `60s` | Delay between collection cycles, minimum 10s |
| `STARLINK_TIMEOUT` | `20s` | Cloud request timeout |
| `STARLINK_LOCAL_TIMEOUT` | `5s` | Local request timeout |
| `STARLINK_STALE_AFTER` | `3m` | Freshness limit; must exceed poll interval + both timeouts |
| `STARLINK_LISTEN` | `:9091` | HTTP listen address |
| `STARLINK_HEALTHCHECK_URL` | `http://127.0.0.1:9091/healthz` | Docker/CLI healthcheck target |
| `STARLINK_MQTT_ENABLED` | `false` | Enable MQTT and HA discovery |
| `STARLINK_MQTT_BROKER` | `tcp://mosquitto:1883` | Broker URL; `ssl://` / `tls://` for TLS |
| `STARLINK_MQTT_USERNAME` | `MQTT_USERNAME` | Broker username |
| `STARLINK_MQTT_PASSWORD` | `MQTT_PASSWORD` | Broker password |
| `STARLINK_MQTT_CLIENT_ID` | `starlink-bridge` | Unique MQTT client ID |
| `STARLINK_MQTT_TOPIC_PREFIX` | `starlinkbridge` | State topic namespace |
| `STARLINK_MQTT_DISCOVERY_PREFIX` | `homeassistant` | HA MQTT discovery prefix |

## Prometheus and Home Assistant

Prometheus metrics have a `starlink_` prefix, use base units, and carry a stable
`device_id` label. Useful series include `starlink_power_watts`,
`starlink_power_mean_watts`, `starlink_current_outage`, `starlink_heating`,
`starlink_thermal_throttle`, and `starlink_outages_window{cause="NO_DOWNLINK"}`.
Window outage counts/durations are gauges, not lifetime counters. No energy total
is invented by repeatedly summing overlapping history buffers. Prometheus can
integrate sampled power externally if needed.

Collector/session metrics include `starlink_bridge_data_fresh`,
`starlink_bridge_auth_required`, `starlink_bridge_poll_errors_total`, and refresh,
expiry and success timestamps when known. The JSON health endpoint never exposes
cookie values. The expiry timestamp is the earliest known cookie expiry, not a
guaranteed expiration time for the login session. Metrics disappear when stale;
health metrics remain available.

Enable MQTT to discover a Starlink dish device and a bridge device in Home
Assistant. Power is in W, rates in Mbit/s, latency in ms and fractions in percent.
Discovery and state are retained, reconnections and HA birth messages republish
discovery, and an MQTT last will marks the bridge offline. Individual missing
measurements are marked unavailable instead of retaining an old reading or
inventing zero. Most supporting sensors are categorized as diagnostics.

## Measurement limits

History is a bounded circular buffer. Power uses its shared sample ordering;
all-zero/absent power histories do not establish zero consumption. Latest power
means the latest returned sample, not proof of real-time freshness. Collection
timestamps report when the bridge fetched data. Outage timestamps are preserved
as raw nanoseconds without assuming an epoch. Status can succeed while history
fails; history-derived readings then become unavailable.

Protobuf core numeric/boolean fields use their schema's zero defaults within a
present status/alerts message. Missing optional messages are unavailable. Firmware
and model support vary; the bridge does not infer numeric SNR, temperature, or
satellite identity from unrelated fields. Physical pointing angles are not a
satellite tracking display.

## Docker and NAS

Use [compose.example.yaml](compose.example.yaml) for a standalone instance. Ensure
the mounted `data/` folder is writable by the container UID (default 10001).
Keep cookies, session state and local configuration outside version control.
See [NAS deployment notes](docs/nas.md) for the optional shared-stack layout,
cookie setup and rebuild steps.

## Development

```powershell
cd bridge
go test ./...
go vet ./...
go build .
```

The module uses standard Go gRPC, Protobuf, Prometheus and MQTT libraries. There
is no third-party Starlink connector or Starlink client dependency.

Protocol references: [Starlink schema](https://github.com/Eitol/starlink-client/tree/a9164c8cf24774c1137d9c28680d8247bf6849eb/proto),
[consumer-cloud transport](https://github.com/Eitol/starlink-client/blob/a9164c8cf24774c1137d9c28680d8247bf6849eb/libs/python/starlink-client/starlink_client/grpc_web_base_client.py),
[history interpretation](https://github.com/sparky8512/starlink-grpc-tools/blob/ea8492fbd38d4da87dd73c726c367b86039430c7/starlink_grpc.py).

## Releases

The release workflow uses GoReleaser v2.19.0 to publish Linux amd64 and arm64
archives with SHA-256 checksums and the multi-platform Docker Hub image
`rcooler/starlink-bridge`. It runs only when a stable `vMAJOR.MINOR.PATCH` tag is
pushed, and requires that tag to point to the current default branch tip. Branch
pushes run CI without publishing a release.

Before the first release, configure the repository Actions secret
`DOCKER_PASSWORD` or `DOCKER_TOKEN` with a Docker Hub access token that can push
`rcooler/starlink-bridge`. The login defaults to `rcooler`; set the
`DOCKER_USERNAME` secret or repository variable if needed. GitHub provides the
workflow's `GITHUB_TOKEN` automatically; no personal GitHub token belongs in the
repository. Starlink cookies and MQTT credentials are not required for releases.

A stable release publishes its full version, major/minor and major image tags,
plus `latest`. Archives include only the binary and explicitly listed public
documentation and configuration examples. The image runs as UID/GID 10001 and
uses the same paths and healthcheck as the source-built image.

Run `goreleaser check` from the repository root to validate the release
configuration. To prepare a local snapshot without publishing, run
`goreleaser release --snapshot --skip=docker`; generated archives go to the
ignored `dist/` directory. The `version` subcommand prints the embedded version.
