# OGInstagram

Instagram embed proxy for Discord, Telegram, and anything that supports Open Graph Protocol or ActivityPub — with rich previews: media, caption, and stats.

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

## Development

Requires a supported Node.js version from `package.json`, Docker, a Cloudflare Workers Paid account with a domain, and a DataImpulse residential proxy plan.

```bash
pnpm install
cp .env.example .env             # proxy, Analytics Engine, and Turnstile credentials
cp .dev.vars.example .dev.vars   # local secrets for `pnpm run dev`

pnpm run dev       # start local dev (Worker + container)
pnpm run check     # type-check + Go tests
pnpm run secrets   # upload .env secrets to Cloudflare
pnpm run deploy    # production
```

> [!NOTE]
> To test Discord or Telegram embeds locally, run `cloudflared tunnel --url http://localhost:8787` and use the tunnel URL in chat. Set `BASE_URL` in `.dev.vars` if embeds point to localhost.


## Configuration

| Variable | Where | Description |
|----------|-------|-------------|
| `PROXY_USERNAME` / `PROXY_PASSWORD` | `.env` | DataImpulse residential proxy credentials |
| `OFFLOAD_SIGNING_KEYS` | secret | JSON keyring used to sign resource-scoped `/offload` capability URLs (14-day expiry) |
| `AE_ACCOUNT_ID` / `AE_API_TOKEN` | `.env` | Analytics Engine access for the status dashboard |
| `BASE_URL` | `wrangler.jsonc` | Public base URL of the deployment |
| `TURNSTILE_SITE_KEY` / `TURNSTILE_SECRET_KEY` | secrets | Turnstile widget and Siteverify credentials |
| `ADMIN_PURGE_TOKEN` | secret | Bearer token for `POST /api/admin/purge` (Workers Caching cache purge) |

The public container uses the external helper fallback; Story routes return
not found without a compatible private implementation.

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
