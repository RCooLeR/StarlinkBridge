# NAS deployment

For a standalone deployment, use [compose.example.yaml](../compose.example.yaml).
The optional staging script targets an existing shared NAS stack with the layout
below. Adapt the stack, network and UID/GID settings to your installation before
using it. No NAS hostname or share is built into the script.

The staging script copies only Go source/build files and documentation, verifies
file hashes, backs up changed configuration, adds `starlink-bridge` to the existing
`80-bridges` Compose stack, and adds a Prometheus scrape job to its live and template
configuration. It does not rebuild or restart containers.

Run from the project root in PowerShell 7 or newer. Pass your own mounted share
or local path as `-NasRoot`; the example hostname below is a placeholder.

```powershell
.\scripts\stage-nas.ps1 -NasRoot '\\nas.example\containers'
```

Optional `-BootstrapCookie PATH` copies a new browser cookie header. Optional
`-SeedCookieState PATH` copies an existing rotated state only if the NAS has none;
an existing NAS session database is never overwritten by staging.

## NAS layout

All paths are relative to the `-NasRoot` argument:

| Purpose | Relative path |
|---|---|
| Build context | `80-bridges/build/starlink-bridge/bridge` |
| Bootstrap cookie | `volumes/80-bridges/starlink-bridge/config/starlink.cookie` |
| Rotating session state | `volumes/80-bridges/starlink-bridge/data/cookies.json` |
| Service settings | `80-bridges/.env` |
| Compose stack | `80-bridges/docker-compose.yaml` |
| Prometheus live config | `volumes/60-observability/prometheus/config/prometheus.yml` |
| Prometheus template | `60-observability/config/prometheus/prometheus.yml` |

The template defaults to cloud mode. Set `STARLINK_MODE` to `local` or `auto` if
the NAS can reach the dish's local endpoint. MQTT uses the existing `MQTT_HOST`,
`MQTT_USERNAME` and `MQTT_PASSWORD` values and the `ugos_shared` Docker network.
The template uses UID/GID `1000:10`; its data directory must be writable by that
user. Port 9091 is exposed only on the shared Docker network.

Keep the real share path, environment values, cookies and backups outside
version control. Configuration backups contain secrets and need the same access
restrictions as the originals.

## Start

1. Save a fresh Starlink Cookie request-header value to the bootstrap path above.
   The file should contain one line; it is not JSON. Keep the incognito account
   session signed in rather than clicking Log out.
2. Rebuild the bridges project as usual. The service can start without a cookie;
   it exposes authentication-required and reads the file when it appears.
3. Reload/restart Prometheus to activate the new `starlink` scrape target.
4. In Home Assistant, check the MQTT integration for **Starlink dish** and
   **Starlink Bridge** devices. MQTT discovery needs no HA configuration edits.

Within the shared Docker network:

```text
http://starlink-bridge:9091/metrics
http://starlink-bridge:9091/api/v1/status
http://starlink-bridge:9091/healthz
http://starlink-bridge:9091/readyz
```

Docker liveness can be healthy while readiness is false (missing cookie,
disconnected dish, or stale data). Do not use a restart loop as an authentication
repair. For an expired/revoked session, replace the bootstrap cookie file; the
bridge imports new contents automatically. Authentication-required can also mean
Starlink denied access, which a new cookie may not resolve. Session rotations
persist under the data volume and survive rebuilds.
