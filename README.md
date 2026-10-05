# OGInstagram

Instagram embed proxy for Discord, Telegram, and other link-preview bots.

## Usage

Replace `instagram.com` in a post, reel, profile, or story link:

| Host | Embed |
|------|-------|
| `oginstagram.com` | Author, caption, stats, media |
| `g.oginstagram.com` | Author and media |
| `d.oginstagram.com` | Media file only |

Select a carousel item with `?img_index=N`. Private and age-restricted posts are not supported.

## Running

```bash
pnpm install --frozen-lockfile
pnpm run dev     # http://localhost:8080
pnpm run check   # lint, types, tests
```

Production runs `compose.yaml` behind a Cloudflare Tunnel (`secrets/tunnel-token`, `data/` owned by UID 65532). Set the values in `.env.example`. Everything else has a default.

## License

[LICENSE](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md)
