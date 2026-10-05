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

Profile links embed follower stats and a grid of recent posts.

> [!NOTE]
> Private posts, age-restricted posts, and posts unavailable in the United States are not supported.

## Architecture

```text
Discord / Telegram / browsers
  → Cloudflare (DNS, static CDN, WAF, Turnstile, Redirect Rules)
  → Cloudflare Tunnel → cloudflared container
  → Go app container on one Vultr VM (gateway, embeds, static web, media previews)
  → SQLite in /data (model cache + metrics, separate proxy-budget ledger)
```

- **Edge:** browser navigations are sent to Instagram by Cloudflare Redirect
  Rules before the WAF; unverified clients are challenged; only verified
  bots reach the origin. The Go app keeps a minimal fallback for the same
  `Sec-Fetch-Mode: navigate` + `Sec-Fetch-Dest: document` signal.
  `www.d.` and `www.g.` are served by a stateless Cloudflare Worker that
  308-redirects to `d.` and `g.` (second-level hosts only get a free
  certificate through Workers Custom Domains).
- **Posts:** the Instagram embed page (direct), then logged-out GraphQL and
  an external helper (through US residential proxies), with oEmbed as the
  last resort. Results race on a schedule, are validated before they win,
  and are cached in memory and SQLite.
- **Profiles:** `web_profile_info` (proxy), then the profile embed page.
  **Stories:** external helper only.
- **Media:** `/offload/*` links are HMAC-signed capabilities (14-day TTL)
  that redirect to Instagram's CDN; the home page preview resizes images to
  WebP/AVIF with FFmpeg.
- **Proxy budget:** DataImpulse traffic has a durable daily byte cap of
  `100,000,000,000 / days in the UTC month`, reserved in 1 MiB leases and
  kept across restarts.

## Deployment

One Vultr High Frequency 1 GB VM (`vhf-1c-1gb`, New Jersey) runs Docker
Compose: the app is capped at 1 CPU / 512 MiB and `cloudflared` at 128 MiB,
with no host ports published. Images are built off the VM and loaded over
SSH; the image tag and the app version are the 8-character commit hash
(`-dirty` when the tree has uncommitted changes).

```bash
pnpm run check
pnpm run image:build    # prints oginstagram:<hash>
docker save oginstagram:<hash> | gzip -1 | ssh linuxuser@<vm> 'gunzip | sudo docker load'
# on the VM: set OG_IMAGE=oginstagram:<hash> in /opt/oginstagram/.env, then
sudo sh tools/deploy.sh # restart, healthcheck, prune old images
sudo sh tools/backup.sh # consistent SQLite snapshot under data/backups
```

The [Vultr deployment and recovery guide](docs/vultr-deployment.md) covers
server setup, secrets, Tunnel hostnames, the live WAF and Redirect rules,
cache ownership, backups, and recovery. There is one app process and one
proxy-budget ledger; never run two app instances against the same `/data`.

## Development

Requires Go 1.26, Node.js (see `package.json` engines), and pnpm. Docker is
needed to test the production image with its bundled Obscura and FFmpeg.

```bash
pnpm install --frozen-lockfile
cp .env.example .env   # set OFFLOAD_SIGNING_KEYS; proxy/helper credentials are optional
pnpm run dev           # build the frontend and serve the Go app on :8080
pnpm run check         # lint, type checks, route/preview tests, Go tests
```

`pnpm run dev` sets `DEVELOPMENT=true`, localhost hosts, a local data
directory, Cloudflare's always-pass Turnstile test site key, and today's UTC
date as the budget start. Restart it after frontend edits. `go run` alone
does not provide the harvester or the WebP/AVIF encoders; the image does.

## Configuration

`.env.example` lists the production variables. Compose sets `PORT`,
`DATA_DIR`, and `ASSETS_DIR`, and mounts the Tunnel token from
`secrets/tunnel-token`. Keep `.env` readable only by the administrator.

| Variables | Purpose |
| --- | --- |
| `OG_IMAGE`, `CLOUDFLARED_IMAGE` | Image tags Compose runs |
| `BASE_URL`, `ALLOWED_HOSTS` | Canonical home URL and accepted public hosts (the `BASE_URL` host must be listed) |
| `TRUSTED_PROXIES` | Exact `cloudflared` address; every other peer is rejected |
| `PROXY_USERNAME`, `PROXY_PASSWORD` | DataImpulse credentials, required together in production |
| `PROXY_BUDGET_START_DATE` | First UTC day this deployment may use the proxy; never move it backward |
| `OFFLOAD_SIGNING_KEYS` | JSON HMAC keyring for `/offload` links |
| `WORKERHUB_SIGN_KEY`, `WORKERHUB_SIGN_TS` | External-helper signing override; rotate together |
| `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET_KEY` | Home page preview widget and Siteverify |
| `ADMIN_PURGE_TOKEN` | Bearer token for `POST /api/admin/purge` (local caches) |

`OFFLOAD_SIGNING_KEYS` uses canonical unpadded base64url 32-byte keys. New
links are signed with `active`; any other key in `keys` still verifies, so
keep a retired key for 14 days after rotating:

```json
{"active":"2026-10","keys":{"2026-10":"<key>","2026-07":"<previous key>"}}
```

Unsigned, malformed, or expired `/offload` links return 404 before any cache
lookup. Dynamic responses are `no-store`, so Cloudflare caches only static
assets. Logs are JSON lines on stderr, rotated by Docker.

The external helper and PP Mori fonts are closed-source. The tracked
`server/external_helper.go` is the public fallback; a private implementation
installs itself at init when present. System fonts replace absent PP Mori
files; Pretendard comes from jsDelivr.

## Preview protection

The home page posts `POST /api/embed` without a token. When Cloudflare answers
`Cf-Mitigated: challenge`, the page runs the Turnstile widget (managed
pre-clearance) and retries once with the token. The app Siteverifies the
token, its action, and that its hostname matches the request host. The WAF
rule `http.request.method eq "POST" and http.request.uri.path eq "/api/embed"`
must stay a Managed Challenge. The `cf_clearance` hash is only a rate-limit
key.

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
