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
npm install
cp .env.example .env             # proxy, Analytics Engine, and Turnstile credentials
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
| `PROXY_HOURLY_LIMIT` | `wrangler.jsonc` | Global proxy requests/hour budget enforced by the Durable Object (`0` = unlimited) |
| `BASE_URL` | `wrangler.jsonc` | Public base URL of the deployment |
| `TURNSTILE_SITE_KEY` / `TURNSTILE_SECRET_KEY` | secrets | Turnstile widget and Siteverify credentials |

The Worker injects the internal Durable Object budget endpoint into the container; `BUDGET_URL` is not user-configurable.

The optional external helper and PP Mori files are intentionally closed-source. Their ignored files may be absent; the standard fetch path and system font fallbacks keep the build functional.

Pretendard and Pretendard JP are loaded as pinned 1.3.9 dynamic subsets from jsDelivr.

## Turnstile deployment invariants

The preview API requires `cf_clearance` and always validates the one-time token with Siteverify, including its action, hostname, client IP, size limit, timeout, and idempotent retry. Before Siteverify, its Workers rate limiter keys clearance-bearing requests by a SHA-256 digest of `cf_clearance`; requests without that cookie are rejected.

Production additionally requires these zone settings:

1. Configure the existing widget for the same registered zone and allowed hostname as the landing page, enable pre-clearance, and select the `managed` clearance level.
2. Add a Managed Challenge WAF rule for `POST /api/embed`. A valid pre-clearance cookie bypasses that challenge.
3. If Advanced Rate Limiting is available, add a rule matching `POST /api/embed`, count by Cookie `cf_clearance`, and block above the chosen request budget. Keep the Workers rate limiter as the application-level fallback.
4. Upload both Turnstile keys with `npm run secrets`; never place the secret in `wrangler.jsonc`.

These settings follow Cloudflare's [pre-clearance requirements](https://developers.cloudflare.com/turnstile/additional-configuration/hostname-management/pre-clearance/), [mandatory Siteverify flow](https://developers.cloudflare.com/turnstile/get-started/server-side-validation/), and [cookie-based rate limiting guidance](https://developers.cloudflare.com/waf/rate-limiting-rules/best-practices/#limit-reuse-of-a-single-cf_clearance-cookie).

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
