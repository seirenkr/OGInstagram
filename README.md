# OGInstagram

**English** | [한국어](README.ko.md)

Instagram embed proxy for Discord, Telegram, and other verified link-preview bots (Discord Component Embeds, Open Graph, and Mastodon/ActivityPub) — with rich previews: media, caption, and stats.

## Usage

Replace `instagram.com` with `oginstagram.com`.

| View | URL | Embeds |
|------|-----|--------|
| Normal | `oginstagram.com`, `www.oginstagram.com` | The creator's profile, caption, stats, and media |
| Gallery | `g.oginstagram.com`, `www.g.oginstagram.com` | The creator's profile and media only |
| Direct | `d.oginstagram.com`, `www.d.oginstagram.com` | Only the direct media URL (posts, reels, stories; profiles get the normal embed) |

Append `?img_index=N` (or `/N` after the shortcode) to pick a carousel item.

### Supported URLs

| Type | Patterns |
|------|----------|
| Posts | `instagram.com/p/…`<br>`instagram.com/username/p/…` |
| Reels | `instagram.com/reel(s)/…`<br>`instagram.com/username/reel(s)/…` |
| User profile | `instagram.com/username` |
| Stories | `instagram.com/stories/username/…` |

Profile links embed follower stats and a grid of recent posts.

### Discord Component Embeds (experimental)

Discordbot HTML requests for posts, reels, profiles, and stories are split
50:50 between the existing Mastodon/ActivityPub preview and a server-rendered
`discord:component-embed` JSON payload. Each request is assigned using its
request ID, including cache hits; Discord's own preview cache can retain the
chosen layout. The `X-OGInstagram-Embed` response header reports the rendered
format, and `discord embed served` logs record both assignment and output.
For testing, `?e=c` pins the Component Embed for Discordbot and `?e=s` also
shows the support button; add any other
parameter (`&n=2`, `&n=3`, …) to bypass Discord's preview cache.
Component Embeds show the author's name in bold and the linked `@username`,
then a blank line and stats, all at the same size beside the avatar, followed by the
caption and media. The footer line shows `OGInstagram` and, for posts and stories
with a known date, a Discord `<t:…:s>` timestamp, with the `📷 Instagram` button
to its right (a Section accessory). Profiles keep the brand footer without a
date. An independent 50% of non-gallery Component Embed requests also add a
`☕ Support me` button linking to [Ko-fi](https://ko-fi.com/seirenkr), as
configured in `.github/FUNDING.yml`; then the brand line stands alone and both
buttons share one row below a separator;
this is about 25% of eligible non-gallery Discord HTML requests. Carousels keep
photos and videos in their original order, up to 10 items; explicit media selection still shows only
the requested item. Gallery links keep the author and media, without captions,
stats, brand footer, timestamps, or either button. Direct-media links keep their redirects.
Discord link labels cannot unescape Markdown, so labels that need escaping
(including some names and caption links) appear as plain text followed by a `↗`
link. The author's name is unlinked bold text; the `@handle` line links to the
profile, because Discord left some names (with emoji) as raw `[label](url)`.
Bare caption URLs are shown once as `<…>` auto-links.

Optional `DISCORD_BRAND_EMOJI_ID` and `DISCORD_VERIFIED_EMOJI_ID` settings accept
numeric Discord custom emoji IDs. The brand icon appears beside `OGInstagram`;
the verified icon appears beside the author's name, outside the link, only when
Instagram's upstream data explicitly reports `is_verified: true`. Missing or
false verification data never receives a badge. Without an emoji ID, the icon
is omitted; no Unicode badge or replacement is added. Older cached models have
no verification flag, so their badge stays absent until the model is refreshed.

The implementation follows [Discord's draft specification](https://github.com/discord/discord-api-docs/pull/8606)
and [FxEmbed's implementation](https://github.com/FxEmbed/FxEmbed/pull/2526).
The final JSON, including escaping and signed media URLs, is limited to 3,000
bytes. Captions are shortened to fit; unusually long URLs can reduce the
gallery size or cause the existing embed to be used instead.

Open Graph and Twitter Card tags remain as fallback. A valid component embed
replaces ActivityPub discovery for Discordbot; other clients and the website
preview keep their existing metadata. Discord's draft and client support can
change, so schema tests do not establish availability in every Discord client.

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
  Rules before the WAF; unverified clients are challenged on embed routes,
  so only verified bots reach the origin's embed paths (the home page,
  static assets, `/api/*` and `/offload/*` are exempt from that rule). The
  Go app keeps a minimal fallback for the same `Sec-Fetch-Mode: navigate` +
  `Sec-Fetch-Dest: document` signal.
  `www.d.` and `www.g.` are served by a stateless Cloudflare Worker that
  308-redirects to `d.` and `g.` (second-level hosts only get a free
  certificate through Workers Custom Domains).
- **Posts:** the Instagram embed page (direct), then logged-out GraphQL and
  an external helper (through US residential proxies), with oEmbed as the
  last resort. Results race on a schedule and are validated before they
  win. Winners are cached in memory; proxy-sourced winners (GraphQL, helper,
  final oEmbed) are also persisted to SQLite.
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
(`pnpm run image:build` refuses an uncommitted tree or a commit not on
`origin/main`). [CONTRIBUTING.md](CONTRIBUTING.md) is the full branch, commit,
release, and rollback process.

```bash
git switch main && git pull --ff-only
pnpm run check
pnpm run image:build    # prints oginstagram:<hash>
docker save oginstagram:<hash> | gzip -1 | ssh linuxuser@<vm> 'gunzip | sudo docker load'
# on the VM: set OG_IMAGE=oginstagram:<hash> in /opt/oginstagram/.env, then
sudo sh tools/deploy.sh # restart, healthcheck, keep current + previous image
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
cp .env.example .env   # set OFFLOAD_SIGNING_KEYS; without PROXY_* only direct embed pages work
pnpm run dev           # build the frontend and serve the Go app on :8080
pnpm run check         # lint, type checks, route/preview tests, Go tests
```

`pnpm run dev` forces `DEVELOPMENT=true`, localhost hosts, a local data
directory and an empty `TRUSTED_PROXIES`; unless `.env` sets them, it also
uses Cloudflare's always-pass Turnstile test site key and today's UTC date
as the budget start. Restart it after frontend edits. Without `PROXY_*`,
GraphQL, oEmbed, the profile API and the external helper (so stories) are
unavailable. `go run` cannot re-harvest helper keys (that needs the image's
Obscura and harvester), and WebP/AVIF previews need an `ffmpeg` with libwebp
and libaom-av1 on `PATH`, which the image bundles.

## Configuration

`.env.example` lists the production variables. Compose sets `PORT`,
`DATA_DIR`, and `ASSETS_DIR`, and mounts the Tunnel token from
`secrets/tunnel-token`. Keep `.env` readable only by the administrator.

| Variables | Purpose |
| --- | --- |
| `OG_IMAGE`, `CLOUDFLARED_IMAGE` | Image tags Compose runs |
| `BASE_URL`, `ALLOWED_HOSTS` | Canonical home URL and accepted public hosts (the `BASE_URL` host must be listed) |
| `TRUSTED_PROXIES` | Exact `cloudflared` address; every other peer is rejected |
| `DEVELOPMENT` | Keep `false` in production; `true` relaxes startup validation |
| `PROXY_USERNAME`, `PROXY_PASSWORD` | DataImpulse credentials; set both or neither, required in production |
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
lookup. Embed, API, `/offload` redirect and error responses are `no-store`;
the home page is `private, no-cache`, and home-preview images are
browser-cacheable but carry `Cloudflare-CDN-Cache-Control: no-store`, so
Cloudflare caches only static assets. Logs are JSON lines on stderr, rotated
by Docker.

The external helper and PP Mori fonts are closed-source. The tracked
`server/external_helper.go` is the public fallback; a private implementation
installs itself at init when present. Without the PP Mori files the font
stack falls back to Pretendard (loaded from jsDelivr) and then system fonts.

## Preview protection

The home page posts `POST /api/embed` without a token. When Cloudflare answers
`Cf-Mitigated: challenge`, the page runs the Turnstile widget (managed
pre-clearance) and retries once with the token. The app Siteverifies the
token, its action, and that its hostname matches the request host. The WAF
rule `http.request.method eq "POST" and http.request.uri.path eq "/api/embed"`
must stay a Managed Challenge. The app requires one `cf_clearance` cookie
but never treats it as proof of clearance; its SHA-256 hash only keys the
8-per-minute preview rate limit (429).

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
