# Cloudflare Tunnel ingress

`cloudflared` already runs on this host and is not defined in
`docker-compose.yml`. Two things have to be true for it to reach navi.

## 1. Shared network

navi's compose file creates a network named `navi`. Either attach the existing
`cloudflared` service to it:

```yaml
# in the cloudflared compose file
services:
  cloudflared:
    networks:
      - default
      - navi

networks:
  navi:
    external: true
```

…or, if `cloudflared` runs on a network of its own, add that network to the
`navi` service instead. Either direction works; what matters is that
`http://navi:8000` resolves from inside the `cloudflared` container.

## 2. Ingress rules

```yaml
ingress:
  - hostname: navi.example.com
    path: ^/(app|api|calendar|webhook|healthz)(/.*)?$
    service: http://navi:8000

  # Everything else on this hostname, including /metrics, is refused at the
  # edge.
  - hostname: navi.example.com
    service: http_status:404

  - service: http_status:404
```

`/metrics` is excluded deliberately. It carries no secrets, but it describes
usage patterns in detail — when reminders fire, how often, how much the models
are being called — and Prometheus scrapes it over the container network, so
nothing needs it from outside the host.

## 3. Protection per path

Set up in the Cloudflare dashboard, not here. From
[docs/03-architecture.md](../docs/03-architecture.md#deployment):

| Path | Protection |
|---|---|
| `/app/*`, `/api/*` | Cloudflare Access, one-time PIN |
| `/calendar/*.ics` | Long random path token |
| `/webhook/*` | Transport-specific shared secret |
| `/healthz` | Open |

Notification actions do not appear in this table. A button tap arrives as a
callback query on `/webhook/telegram`, already authenticated by the shared secret
and already filtered by the sender allowlist (D8, D-006). There is no
session-less public action path in this service.

Only `/healthz` exists today. The rest are listed so the ingress rule does not
need revisiting as each one lands.
