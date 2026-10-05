# OGInstagram

**English** | [한국어](README.ko.md)

Instagram embed proxy for Discord, Telegram, and other link-preview bots: media, caption, and stats.

## Usage

Replace `instagram.com` with one of these hosts:

| Host | Embed |
|------|-------|
| `oginstagram.com` | Author, caption, stats, media |
| `g.oginstagram.com` | Author and media only |
| `d.oginstagram.com` | Direct media URL (profiles get the normal embed) |

Supported paths: `/p/…`, `/reel/…`, `/reels/…` (optionally prefixed with `/username`), `/username`, `/stories/username/…`. Pick a carousel item with `?img_index=N` or `/N`.

Private, age-restricted, and US-unavailable posts are not supported.

### Discord Component Embeds

Discordbot requests are split 50:50 between the Mastodon/ActivityPub preview and a [Component Embed](https://github.com/discord/discord-api-docs/pull/8606). Half of the Component Embeds add a Ko-fi support button. Open Graph tags remain as the fallback.

- `?e=c` forces the Component Embed and `?e=s` adds the support button. Add any parameter (`&n=2`) to bypass Discord's preview cache.
- The `X-OGInstagram-Embed` header and the `discord embed served` log line report which format was served.
- The payload must stay under 3,000 bytes, so long captions are truncated.

## Architecture

```text
Bots → Cloudflare (WAF, Redirect Rules, Turnstile) → Tunnel → Go app on one Vultr VM → SQLite (/data)
```

- **Edge:** Cloudflare redirects browsers to Instagram and challenges unverified clients, so only verified bots reach the embed routes.
- **Posts:** the Instagram embed page first, then GraphQL and an external helper through US residential proxies, with oEmbed last.
- **Profiles:** `web_profile_info`, then the profile embed page. **Stories:** the external helper only.
- **Media:** `/offload/*` links are HMAC-signed (14-day TTL) and redirect to Instagram's CDN.
- **Proxy budget:** a durable daily byte cap (100 GB per month, split by day).

## Deployment

Releases follow [CONTRIBUTING.md](CONTRIBUTING.md): build from `main` only, tag the image with the 8-character commit hash, and keep the previous image for rollback. Server setup, secrets, WAF rules, backups, and recovery are in [docs/vultr-deployment.md](docs/vultr-deployment.md).

## Development

Requires Go 1.26, Node.js (see `package.json` engines), and pnpm.

```bash
pnpm install --frozen-lockfile
pnpm run dev           # frontend build + Go app on :8080
pnpm run check         # lint, types, tests
```

Without `PROXY_*` set, only the direct embed pages work. Helper key re-harvesting and WebP/AVIF previews need the Docker image, which bundles Obscura and FFmpeg.

## Configuration

Production `.env` needs only `OG_IMAGE`, `PROXY_USERNAME`, `PROXY_PASSWORD`, `TURNSTILE_SITE_KEY`, and `TURNSTILE_SECRET_KEY` (see `.env.example`). Everything else has a default. The offload signing keyring is generated in `/data` on first start. Optional overrides are listed in [docs/vultr-deployment.md](docs/vultr-deployment.md).

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
