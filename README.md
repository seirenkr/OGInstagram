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

The Worker injects the internal cache and budget endpoints into the container; `CACHE_URL` and `BUDGET_URL` are not user-configurable. Proxy traffic has a Durable Object-backed daily byte cap calculated as `100,000,000,000 / days in the UTC month`. The Go proxy transport counts every byte read from or written to the proxy connection and uses 1 MiB process-local byte leases to avoid a storage round trip for every socket operation. Unused daily capacity does not carry forward. Independently, each Instagram proxy session remains limited to 1,000 requests per fixed one-hour window.

`OFFLOAD_SIGNING_KEYS` must contain an active key ID and one or more canonical,
unpadded base64url-encoded 32-byte keys:

```json
{"active":"2026-07","keys":{"2026-07":"<32-byte base64url key>"}}
```

Generate each key from a cryptographically secure source (for example,
`openssl rand -base64 32`, converted to unpadded base64url). Rotation is
additive: deploy a keyring containing both old and new keys before changing
`active`, and retain old keys for at least 14 days so links signed just
before rotation keep verifying until they expire. Never place the keyring
in `wrangler.jsonc`.

Signed `/offload` links carry a maximum 14-day expiry. The gateway rejects
unsigned, partially signed, expired, or longer-lived links with a `404` before
any cache lookup, regardless of that link's own edge-cache state. Re-sending
the original message reissues a fresh capability that normally still hits a
warm model cache instead of a full Instagram re-fetch.

### Purging the edge cache

Workers Caching keeps entries across deploys (`cross_version_cache`), so routine deploys keep a warm cache. After a deploy that changes caching behavior itself (cache key shape, TTL rules, what counts as cacheable), purge manually:

```bash
curl -X POST https://oginstagram.com/api/admin/purge -H "Authorization: Bearer $ADMIN_PURGE_TOKEN"
```

The optional external helper, its runtime integration, and PP Mori files are
intentionally closed-source and excluded from the public container build.
The tracked `server/external_helper.go` always supplies the public
fallback seam. System font fallbacks cover missing PP Mori files.
Self-hosted Story fetching requires a compatible external helper
implementation; without one, Story routes return not found.

Pretendard and Pretendard JP are loaded as pinned 1.3.9 dynamic subsets from jsDelivr.

## Turnstile deployment invariants

The preview API requires `cf_clearance` and always validates the one-time
Turnstile token with Siteverify, including its action, hostname, client IP,
size limit, timeout, and idempotent retry. The Worker does not and cannot
authenticate the opaque clearance cookie itself: the zone's Managed Challenge
is the validation boundary. After that boundary, the Worker uses a SHA-256
digest of the cookie only as an application rate-limit key and rejects missing
or ambiguous cookie values.

Production additionally requires these zone settings:

1. Configure the existing widget for the same registered zone and allowed hostname as the landing page, enable pre-clearance, and select the `managed` clearance level.
2. Add a Managed Challenge WAF rule for `POST /api/embed`. This rule is
   mandatory: Cloudflare accepts a valid pre-clearance cookie at the challenge
   layer before the request reaches the Worker.
3. If Advanced Rate Limiting is available, add a rule matching `POST /api/embed`, count by Cookie `cf_clearance`, and block above the chosen request budget. Keep the Workers rate limiter as the application-level fallback.
4. Upload both Turnstile keys with `pnpm run secrets`; never place the secret in `wrangler.jsonc`.

These settings follow Cloudflare's [pre-clearance requirements](https://developers.cloudflare.com/turnstile/additional-configuration/hostname-management/pre-clearance/), [mandatory Siteverify flow](https://developers.cloudflare.com/turnstile/get-started/server-side-validation/), and [cookie-based rate limiting guidance](https://developers.cloudflare.com/waf/rate-limiting-rules/best-practices/#limit-reuse-of-a-single-cf_clearance-cookie).

## Acknowledgements

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
