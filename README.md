# OGInstagram

Instagram embed proxy for Discord, Telegram, and anything that supports Open Graph Protocol or ActivityPub — with rich previews: media, caption, and stats.

## Usage

Replace `instagram.com` with `oginstagram.com`.

| View | URL | Embeds |
|------|-----|--------|
| Normal | `oginstagram.com` | The creator's profile, caption, stats, and media |
| Gallery | `g.oginstagram.com` | The creator's profile and media only |
| Direct | `d.oginstagram.com` | Only the direct media URL |

Append `?img_index=N` (or `/N` after the shortcode) to pick a carousel item.

### Supported URLs

| Type | Patterns |
|------|----------|
| Posts | `instagram.com/p/…`<br>`instagram.com/username/p/…` |
| Reels | `instagram.com/reel(s)/…`<br>`instagram.com/username/reel(s)/…` |
| User profile | `instagram.com/username` |

Profile links embed the bio, follower stats, and a grid of recent posts.

> [!NOTE]
> Private posts, age-restricted posts, and posts unavailable in the United States are not supported.

## Development

Requires Node.js 22.9+, Docker, a Cloudflare Workers Paid account with a domain, and a DataImpulse residential proxy plan.

```bash
npm install
cp .env.example .env             # proxy + Analytics Engine credentials
cp .dev.vars.example .dev.vars   # local secrets for `npm run dev`

npm run dev       # start local dev (Worker + container)
npm run check     # type-check + Go tests
npm run secrets   # upload .env secrets to Cloudflare
npm run deploy    # production
```

> [!NOTE]
> To test Discord or Telegram embeds locally, run `cloudflared tunnel --url http://localhost:8787` and use the tunnel URL in chat. Set `BASE_URL` in `.dev.vars` if embeds point to localhost.


## Configuration

| Variable | Where | Description |
|----------|-------|-------------|
| `PROXY_USERNAME` / `PROXY_PASSWORD` | `.env` | DataImpulse residential proxy credentials |
| `AE_ACCOUNT_ID` / `AE_API_TOKEN` | `.env` | Analytics Engine access for the status dashboard |
| `PROXY_HOURLY_LIMIT` | container env (optional) | Global proxy requests/hour budget (default 2500; `0` = unlimited) |
| `BASE_URL` | `wrangler.jsonc` | Public base URL of the deployment |
| `BRAND_NAME` / `BRAND_COLOR` | `wrangler.jsonc` | Branding for previews and the landing page |
| `SUPPORT_URL` / `GITHUB_URL` | `wrangler.jsonc` | Footer and call-to-action links |

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
