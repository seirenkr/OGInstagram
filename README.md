# OGInstagram

Instagram embed proxy for Discord, Telegram, and anything that supports Open Graph Protocol or ActivityPub — with rich previews: media, caption, and stats.

## Usage

Given a post at `https://instagram.com/p/CODE/`:

| View | URL |
|------|-----|
| Normal | `https://oginstagram.com/p/CODE/` |
| Gallery | `https://g.oginstagram.com/p/CODE/` |
| Direct media | `https://d.oginstagram.com/p/CODE/` |

Works with posts (`/p/…`), reels (`/reel/…`), and user posts/reels (`/username/…`). Append `?img_index=N` to pick a carousel item.

> [!NOTE]
> Private, age-restricted, and US-unavailable posts are not supported.

## Development

Requires Node.js 22.9+, Docker, a Cloudflare Workers Paid account with a domain, and a Decodo residential proxy plan.

```bash
npm install
cp .env.example .env             # proxy + Analytics Engine credentials
cp .dev.vars.example .dev.vars   # local secrets for `npm run dev`

npm run dev       # full stack locally (Worker + container)
npm run check     # type-check + Go tests
npm run preview   # upload a version, get a public preview URL
npm run secrets   # upload .env secrets to Cloudflare
npm run deploy    # production
```

To test crawlers (Discord, Telegram) against a local run, expose it with a Cloudflare Tunnel and set `BASE_URL` in `.dev.vars` to the tunnel URL.

## Configuration

| Variable | Where | Description |
|----------|-------|-------------|
| `DECODO_USERNAME` / `DECODO_PASSWORD` | `.env` | Residential proxy credentials |
| `AE_ACCOUNT_ID` / `AE_API_TOKEN` | `.env` | Analytics Engine access for the status dashboard |
| `BASE_URL` | `wrangler.jsonc` | Public base URL of the deployment |
| `BRAND_NAME` / `BRAND_COLOR` | `wrangler.jsonc` | Branding for previews and the landing page |
| `SUPPORT_URL` / `GITHUB_URL` | `wrangler.jsonc` | Footer and call-to-action links |

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [Lainmode/InstagramEmbed-vxinstagram](https://github.com/Lainmode/InstagramEmbed-vxinstagram)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
