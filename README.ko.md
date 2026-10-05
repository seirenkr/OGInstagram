# OGInstagram

[English](README.md) | **한국어**

Discord, Telegram 등 링크 미리보기 봇에 미디어·캡션·통계가 담긴 미리보기를 보여 주는 Instagram 임베드 프록시입니다.

## 사용법

`instagram.com`을 다음 호스트로 바꿉니다.

| 호스트 | 임베드 |
|--------|--------|
| `oginstagram.com` | 작성자, 캡션, 통계, 미디어 |
| `g.oginstagram.com` | 작성자와 미디어만 |
| `d.oginstagram.com` | 미디어 직접 URL (프로필은 일반 임베드) |

지원 경로는 `/p/…`, `/reel/…`, `/reels/…`(앞에 `/username`을 붙여도 됨), `/username`, `/stories/username/…`입니다. 캐러셀 항목은 `?img_index=N` 또는 `/N`으로 고릅니다.

비공개 게시물, 연령 제한 게시물, 미국에서 볼 수 없는 게시물은 지원하지 않습니다.

### Discord Component Embed

Discordbot 요청은 Mastodon/ActivityPub 미리보기와 [Component Embed](https://github.com/discord/discord-api-docs/pull/8606)로 50:50 나뉩니다. Component Embed 중 절반에는 Ko-fi 후원 버튼이 붙습니다. Open Graph 태그는 대체용으로 남아 있습니다.

- `?e=c`를 붙이면 Component Embed로, `?e=s`를 붙이면 후원 버튼까지 고정됩니다. Discord 미리보기 캐시를 우회하려면 파라미터를 하나 더 붙입니다(`&n=2`).
- 어떤 형식이 나갔는지는 `X-OGInstagram-Embed` 헤더와 `discord embed served` 로그로 확인합니다.
- 페이로드는 3,000바이트를 넘을 수 없어서 긴 캡션은 잘립니다.

## 구조

```text
봇 → Cloudflare (WAF, Redirect Rules, Turnstile) → Tunnel → Vultr VM의 Go 앱 → SQLite (/data)
```

- **엣지:** Cloudflare가 브라우저는 Instagram으로 리다이렉트하고 검증되지 않은 클라이언트에는 challenge를 걸기 때문에, 임베드 경로에는 검증된 봇만 들어옵니다.
- **게시물:** Instagram 임베드 페이지를 먼저 시도하고, 이어서 미국 주거용 프록시를 거쳐 GraphQL과 외부 helper를, 마지막으로 oEmbed를 시도합니다.
- **프로필:** `web_profile_info`, 실패하면 프로필 임베드 페이지. **스토리:** 외부 helper만 사용합니다.
- **미디어:** `/offload/*` 링크는 HMAC 서명(유효기간 14일)이 붙어 있고 Instagram CDN으로 리다이렉트합니다.
- **프록시 예산:** 일별 바이트 한도(월 100 GB를 날짜 수로 나눔)를 DB에 저장해 재시작해도 유지합니다.

## 배포

릴리스는 [CONTRIBUTING.md](CONTRIBUTING.md)를 따릅니다. `main`에서만 빌드하고, 이미지 태그는 8자리 커밋 해시로 붙이며, 롤백용으로 직전 이미지를 남깁니다. 서버 준비, 비밀값, WAF 규칙, 백업과 복구는 [docs/vultr-deployment.md](docs/vultr-deployment.md)에 있습니다.

## 개발

Go 1.26, Node.js(`package.json`의 engines 참고), pnpm이 필요합니다.

```bash
pnpm install --frozen-lockfile
cp .env.example .env   # OFFLOAD_SIGNING_KEYS 설정
pnpm run dev           # 프론트엔드 빌드 + :8080에서 Go 앱 실행
pnpm run check         # lint, 타입, 테스트
```

`PROXY_*`를 설정하지 않으면 직접 가져오는 임베드 페이지만 동작합니다. helper 키 재수집과 WebP/AVIF 미리보기에는 Obscura와 FFmpeg가 들어 있는 Docker 이미지가 필요합니다.

## 설정

모든 변수는 `.env.example`에 있습니다.

| 변수 | 용도 |
| --- | --- |
| `OG_IMAGE`, `CLOUDFLARED_IMAGE` | Compose가 실행할 이미지 |
| `BASE_URL`, `ALLOWED_HOSTS` | 대표 URL과 허용할 호스트 |
| `TRUSTED_PROXIES` | `cloudflared` 주소. 이 주소 외의 요청은 거부 |
| `PROXY_USERNAME`, `PROXY_PASSWORD` | DataImpulse 자격 증명 (운영 필수) |
| `PROXY_BUDGET_START_DATE` | 프록시를 처음 쓸 수 있는 UTC 날짜. 절대 앞당기지 않음 |
| `OFFLOAD_SIGNING_KEYS` | `/offload` 링크용 HMAC 키 모음 |
| `WORKERHUB_SIGN_KEY`, `WORKERHUB_SIGN_TS` | 외부 helper 서명값을 직접 지정 |
| `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET_KEY` | 홈페이지 미리보기 보호 |
| `ADMIN_PURGE_TOKEN` | `POST /api/admin/purge`용 Bearer 토큰 |
| `DISCORD_BRAND_EMOJI_ID`, `DISCORD_VERIFIED_EMOJI_ID` | 선택. 커스텀 이모지 ID ([에셋](docs/discord-emojis/README.md)) |

`OFFLOAD_SIGNING_KEYS`의 키는 32바이트를 패딩 없는 base64url로 적습니다. 새 링크는 `active` 키로 서명합니다. 키를 교체한 뒤에도 이전 키는 14일 동안 남겨 둡니다.

```json
{"active":"2026-10","keys":{"2026-10":"<key>","2026-07":"<previous key>"}}
```

## 감사의 말

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
