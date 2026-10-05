# OGInstagram

Instagram embed proxy for Discord, Telegram, and other verified link-preview bots (Open Graph, plus the Mastodon/ActivityPub endpoints Discord uses) — with rich previews: media, caption, and stats.

## Usage

Replace `instagram.com` with `oginstagram.com`.

| View | URL | Embeds |
|------|-----|--------|
| Normal | `oginstagram.com`, `www.oginstagram.com` | The creator's profile, caption, stats, and media |
| Gallery | `g.oginstagram.com`, `www.g.oginstagram.com` | The creator's profile and media only |
| Direct | `d.oginstagram.com`, `www.d.oginstagram.com` | Only the direct media URL |

Append `?img_index=N` (or `/N` after the shortcode) to pick a carousel item.

### Supported URLs

| Type | Patterns |
|------|----------|
| Posts | `instagram.com/p/…`<br>`instagram.com/username/p/…` |
| Reels | `instagram.com/reel(s)/…`<br>`instagram.com/username/reel(s)/…` |
| User profile | `instagram.com/username` |
| Stories | `instagram.com/stories/username/…` |

Profile links embed the bio, follower stats, and a grid of recent posts.

> [!NOTE]
> Private posts, age-restricted posts, and posts unavailable in the United States are not supported.

## Deployment

The complete application runs on one **Vultr High Frequency 1 GB
(`vhf-1c-1gb`) in New Jersey (`ewr`)**, with 1 shared vCPU, 1,024 MB RAM,
32 GB NVMe, and 1,024 GB monthly transfer. The VM costs **$6/month before
tax**, or an expected **$6.60 with 10% Korean VAT**. Automatic provider backup
is optional and excluded by default; enabling it adds 20% ($7.20 before tax,
$7.92 with Korean VAT). The app remains capped at **1 CPU and 512 MiB**, with
128 MiB for `cloudflared`. Use one VM without paid addons. See the
[official plan API](https://api.vultr.com/v2/plans) and
[tax policy](https://docs.vultr.com/support/platform/billing/does-vultr-collect-vat-or-sales-tax).
Cloudflare provides DNS, static asset CDN, WAF, Turnstile, and Tunnel ingress.
The application does not require Workers, Containers, KV, Durable Objects, or
Analytics Engine. Production deployment uses Docker Compose and a prebuilt
image; build images away from the small VM.

Follow [the Vultr deployment and recovery guide](docs/vultr-deployment.md) for
Ubuntu 24.04, secrets, all six public hostnames, the WAF and redirect rules, cold
cutover, backups, and recovery. Deployment intentionally stops the old service
before starting the replacement. There is one app process and one durable
proxy-budget ledger.

## Development

Requires Go 1.26, a supported Node.js version from `package.json`, and pnpm.
Docker is needed to test the production image and its bundled Obscura/FFmpeg.

```bash
pnpm install --frozen-lockfile
cp .env.example .env
# Set OFFLOAD_SIGNING_KEYS; proxy/helper credentials are optional locally.
pnpm run dev       # build the frontend and serve the complete Go app on :8080
pnpm run check     # frontend lint/types, browser URL tests, and Go tests
pnpm run image:build
```

`pnpm run dev` uses `DEVELOPMENT=true`, a local data directory, localhost
hostnames, and the current UTC date for a fresh local proxy budget. Production
always uses `DEVELOPMENT=false`. Restart development after frontend edits;
`pnpm run build` also builds the eight localized home pages and local emoji
assets. The image includes a harvester runtime and real WebP/AVIF encoders;
`go run` alone does not supply those executables.

## Configuration

`.env.example` documents the production environment. Keep `.env` readable only
by the deployment administrator and keep the Tunnel token in
`secrets/tunnel-token`, not a command argument. `compose.yaml` mounts `/data`
for persistent SQLite and `/tmp` as bounded temporary memory, runs UID 65532,
and publishes no host ports; cloudflared reaches `app:8080` on the Compose network. Normal startup verifies that all
eight localized home templates exist; storage maintenance commands do not
require frontend assets. Partial proxy credentials are rejected in every mode.

| Variables | Purpose |
| --- | --- |
| `BASE_URL`, `ALLOWED_HOSTS` | Canonical home URL and accepted public hosts |
| `TRUSTED_PROXIES` | Exact connector IP allowed to supply Cloudflare client metadata |
| `DATA_DIR`, `ASSETS_DIR` | SQLite directory and built frontend assets |
| `PROXY_USERNAME`, `PROXY_PASSWORD` | Required together in production; development may omit both |
| `PROXY_BUDGET_START_DATE` | Required earliest UTC day for a cold deployment's proxy use |
| `OFFLOAD_SIGNING_KEYS` | New or explicitly selected JSON HMAC keyring for 14-day media links |
| `WORKERHUB_SIGN_KEY`, `WORKERHUB_SIGN_TS` | External-helper signing override; rotate together |
| `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET_KEY` | Preview pre-clearance widget and Siteverify |
| `ADMIN_PURGE_TOKEN` | Bearer token for `POST /api/admin/purge` (local model and preview caches) |

Models and the status page use bounded local SQLite storage. DataImpulse
traffic has a durable daily byte cap of `100,000,000,000 / days in the UTC
month`; unused capacity does not carry forward. The Go proxy transport counts
all socket bytes and reserves 1 MiB leases. The proxy ledger survives restarts.
Restoring an older backup must exhaust the current UTC day's budget before
traffic resumes; see the recovery guide. Vultr's public media transfer is a
separate allowance and cost.

`OFFLOAD_SIGNING_KEYS` has this shape, with canonical unpadded base64url
32-byte keys:

```json
{"active":"2026-07","keys":{"2026-07":"<32-byte base64url key>"}}
```

Generate a fresh keyring for this deployment, or explicitly choose to reuse an
existing one. Prior deployment links do not need to remain compatible.
Unsigned, malformed, expired, or longer-lived `/offload` links return 404
before the model or transformed image cache is consulted. Dynamic responses are
`no-store`, so Cloudflare caches only static assets; no Cache Rule is needed.

The optional external helper and PP Mori files are intentionally closed-source.
The tracked `server/external_helper.go` supplies the public fallback seam;
when the private implementation is present, its package initializer installs
the helper automatically. System font fallbacks cover absent PP Mori files.
Pretendard and Pretendard JP use pinned 1.3.9 dynamic subsets from jsDelivr.

## Preview protection

The browser posts `POST /api/embed` without a token and executes the existing
Turnstile widget when `Cf-Mitigated: challenge` is returned. Its callback
retries once with the generated token. The application Siteverifies supplied
tokens and checks the action and that the hostname matches the request host. Later tokenless requests
rely on Cloudflare's WAF validating pre-clearance before Tunnel ingress.
The cookie hash is a rate-limit key, not an application authentication proof.

Keep the widget's managed pre-clearance and this WAF Managed Challenge rule:

```text
http.request.method eq "POST" and http.request.uri.path eq "/api/embed"
```

Keep the origin private and `DEVELOPMENT=false`. Forged Cloudflare or internal
headers from an untrusted peer must never authorize a production preview or
origin media fetch.

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
