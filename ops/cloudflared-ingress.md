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

**Port 8000 on the host is not navi's.** Another service on this host owns it.
That does not matter to the tunnel: `navi:8000` is the container's own port on
the compose network, a different address from the host's `127.0.0.1:8000`. Only
the host-side publish moved, to `127.0.0.1:${NAVI_HOST_PORT:-8010}`, which is
for `curl` and debugging and which the tunnel never uses. The exception is a
`cloudflared` running directly on the host rather than in Docker: then it cannot
resolve `navi`, and the rule's `service:` must be `http://localhost:8010`.

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
nothing needs it from outside the host. Litestream's own metrics (session 7)
are on a separate port, `navi:9200`, which is never named in the `service:`
line above at all — there is no path that could reach it through this
ingress, the same exclusion `/metrics` gets, for the same reason.

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

`/healthz`, `/webhook/telegram`, the `/api` resolve, snooze and `GET /api/today`
routes and — since session 20 — the `/app` day view exist today. The ingress
regex above already matches `/app`, `/app/` and everything under it, so no rule
changes; what must be true is that the Access policy for `/app/*` is applied in
the dashboard (it is the same policy as `/api/*`). The web manifest is fetched
with credentials (`crossorigin="use-credentials"`) so Access does not redirect it
to the login page. The remaining routes (calendar, stats) are listed so the
ingress rule does not need revisiting as each one lands.

### Dashboard walkthrough: Access with an emailed one-time PIN

Do this **before** pointing the tunnel at navi. Replace `navi.example.com` with
your hostname and `you@example.com` with your email.

1. **Open Zero Trust.** Cloudflare dashboard → *Zero Trust* (one.dash.cloudflare.com).
   First time only: pick a team name and the Free plan (free up to 50 users).
2. **Enable the PIN login method.** *Settings → Authentication → Login methods*.
   *One-time PIN* is normally present by default; if not, *Add new* → *One-time PIN*
   → *Save*.
3. **Create the policy once, reusable.** *Access controls → Policies → Add a policy*.
   - Name: `navi-owner-only`
   - Action: **Allow**
   - Include rule: selector **Emails**, value `you@example.com`
   - Leave Require/Exclude empty. Session duration: pick e.g. 1 month, so you
     are not re-prompted constantly on the phone. Save.
   Nothing else can match: an address not on the list is never sent a code.
4. **Create the application for `/app`.** *Access controls → Applications →
   Add an application → Self-hosted*.
   - Application name: `navi-app`
   - Session duration: 1 month
   - *Add public hostname*: Domain `navi.example.com`, Path `app`
     (and add a second entry with Path `app/*` if the dashboard does not match
     subpaths for you).
   - Policies: select `navi-owner-only`.
   - Login methods: untick everything but *One-time PIN*; skip the "instant auth"
     option.
   - Save.
5. **Create the application for `/api`.** Same again: name `navi-api`, hostname
   `navi.example.com`, Path `api` (and `api/*`), policy `navi-owner-only`.
   (Alternatively add both paths as destinations of one application. Either works.)
6. **Do not cover `/webhook`, `/healthz` or `/calendar`.** Telegram cannot pass an
   email PIN, so `/webhook/telegram` has to stay outside Access; it is protected
   by its shared secret and the sender allowlist. Access only guards the paths
   you named, so leaving them out is the whole configuration. Do not create a
   hostname-wide application.
7. **Add the tunnel's public hostname.** *Networks → Tunnels →* your tunnel →
   *Edit → Published application routes (Public Hostname) → Add*:
   Hostname `navi.example.com`, Service type `HTTP`, URL `navi:8000`. If you
   manage ingress through a local `config.yml` instead, use the rules in
   section 2 and skip this step. Save.
8. **Verify, once, by hand.**
   - Private window → `https://navi.example.com/app/`: you should land on a
     Cloudflare login asking for an email. Enter yours, then the 6-digit code
     from your inbox (check spam; codes expire after 10 minutes). You should
     then see the day view.
   - `curl -i https://navi.example.com/api/today` with no cookie: expect a
     `302` to `cloudflareaccess.com` (or `403`), **not** JSON.
   - `curl -i https://navi.example.com/metrics`: expect `404`.
   - `curl -i https://navi.example.com/healthz`: expect `200`.
   - Telegram: send the bot a message; it should still reply (the webhook
     is unaffected).
9. **Set the Telegram webhook** only after steps 4–8 pass, to
   `https://navi.example.com/webhook/telegram`.

Tip: if the installed PWA ever shows a login page instead of the app, open it
once in the browser to refresh the Access session; the manifest is already
fetched with credentials so it does not trip the redirect.

**`/api` carries no in-process authentication, by design.** The container trusts
that a request reaching `/api/*` came through this tunnel with an Access session
already established, which is what D-014 means by "auth is resolved before the
handler runs" and what makes one resolution endpoint serve three surfaces at no
per-surface cost. Two things follow. The Access policy for `/api/*` must
actually be applied in the dashboard before the tunnel is pointed at a real
host — an ingress rule without it publishes the resolution endpoint. And the
container port must not be reachable except through the tunnel, since anything
that can open a socket to it can resolve any occurrence by id.

Still unverified against the real tunnel: that `/api` is reachable through it at
all, and that an unauthenticated request is refused at the edge. Neither is code
work, and both are in the same class as session 7's open reachability item —
they need the real host, and they should be checked once by hand on the first
deploy rather than assumed.
