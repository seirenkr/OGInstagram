# OGInstagram

[English](README.md) | **한국어**

Discord, Telegram 등 검증된 링크 미리보기 봇을 위한 Instagram 임베드 프록시입니다. Discord Component Embed, Open Graph, Mastodon/ActivityPub을 제공하며, 미디어·캡션·통계가 담긴 풍부한 미리보기를 보여 줍니다.

## 사용법

링크의 `instagram.com`을 `oginstagram.com`으로 바꾸면 됩니다.

| 보기 | URL | 임베드 내용 |
|------|-----|--------|
| 일반 | `oginstagram.com`, `www.oginstagram.com` | 작성자 프로필, 캡션, 통계, 미디어 |
| 갤러리 | `g.oginstagram.com`, `www.g.oginstagram.com` | 작성자 프로필과 미디어만 |
| 다이렉트 | `d.oginstagram.com`, `www.d.oginstagram.com` | 미디어 원본 URL만 (게시물·릴스·스토리. 프로필은 일반 임베드) |

캐러셀의 특정 항목을 고르려면 `?img_index=N`을 붙이거나 shortcode 뒤에 `/N`을 붙입니다.

### 지원하는 URL

| 종류 | 패턴 |
|------|----------|
| 게시물 | `instagram.com/p/…`<br>`instagram.com/username/p/…` |
| 릴스 | `instagram.com/reel(s)/…`<br>`instagram.com/username/reel(s)/…` |
| 프로필 | `instagram.com/username` |
| 스토리 | `instagram.com/stories/username/…` |

프로필 링크는 팔로워 통계와 최근 게시물 그리드를 보여 줍니다.

### Discord Component Embed (실험 기능)

Discordbot의 게시물·릴스·프로필·스토리 HTML 요청을 50:50으로 나누어
기존 Mastodon/ActivityPub 미리보기와 `discord:component-embed` JSON을
제공합니다. 모델 캐시 적중 여부와 무관하게 요청 ID로 배정하며,
Discord 자체 미리보기 캐시에는 선택된 레이아웃이 남을 수 있습니다.
`X-OGInstagram-Embed` 응답 헤더는 실제 형식을 알려 주고,
`discord embed served` 로그는 배정 결과와 실제 출력을 기록합니다.
테스트할 때는 `?e=c`로 Discordbot에 Component Embed를, `?e=s`로 Support 버튼까지 고정할 수 있고, 다른 파라미터를 덧붙이면(`&n=2`, `&n=3`, …) Discord 미리보기 캐시를 우회합니다.
Component Embed는 아바타 옆에 굵은 이름과 링크된 `@사용자명`, 한 줄 띄운 통계를 같은 크기로
표시하고, 그 뒤로 캡션과 미디어를 보여 줍니다.
마지막 줄에는 `OGInstagram` 브랜드와 게시물·스토리의 알려진 게시 시각을
Discord의 `<t:…:s>` 형식으로 표시하고, 오른쪽에 원문으로 연결하는
`📷 Instagram` 버튼을 Section accessory로 붙입니다. 프로필은 시각 없이 브랜드를 표시합니다.
갤러리를 제외한 Component Embed 요청의 50%에는 독립적으로 `☕ Support me`
버튼을 추가하여 `.github/FUNDING.yml`의 [Ko-fi](https://ko-fi.com/seirenkr)로
연결합니다. 이때는 브랜드 줄만 두고, 구분선 아래 한 행에 두 버튼을 나란히 놓습니다. 갤러리를 제외한 전체 대상 Discord HTML 요청의 약 25%에 해당합니다.
캐러셀은 사진과 영상을 원래
순서대로 최대 10개까지 담으며, 항목을 지정하면 해당 미디어만 표시합니다.
갤러리 링크는 작성자와 미디어만 담고 캡션·통계·브랜드·게시 시각과 두 버튼을 모두 숨깁니다.
다이렉트 미디어 링크의 리다이렉트는 유지합니다.
Discord 링크 라벨은 Markdown 이스케이프를 해제하지 못하므로, 일부 이름·캡션 링크처럼
이스케이프가 필요한 라벨은 일반 텍스트와 뒤따르는 `↗` 링크로 표시합니다.
작성자 이름은 링크 없는 굵은 글씨로 두고 `@사용자명` 줄에 프로필 링크를 겁니다.
이모지가 든 일부 이름에서 Discord가 `[라벨](url)`을 그대로 노출했기 때문입니다.
캡션의 일반 URL은 `<…>` 자동 링크로 한 번만 표시합니다.

선택 설정인 `DISCORD_BRAND_EMOJI_ID`와 `DISCORD_VERIFIED_EMOJI_ID`에는
Discord 커스텀 이모지의 숫자 ID를 지정합니다. 브랜드 아이콘은 `OGInstagram`
옆에, 인증 아이콘은 이름 링크 밖에 표시합니다. 인증 아이콘은 Instagram 원본
데이터가 `is_verified: true`를 명시한 계정에만 표시하며, false이거나 정보가
없으면 표시하지 않습니다. 이모지 ID를 설정하지 않으면 아이콘을 생략하고
유니코드 배지나 다른 기호로 대체하지 않습니다. 기존 캐시에는 인증 정보가
없으므로 모델을 다시 가져올 때까지 인증 아이콘이 나타나지 않습니다.

[Discord 예비 문서](https://github.com/discord/discord-api-docs/pull/8606)와
[FxEmbed 구현](https://github.com/FxEmbed/FxEmbed/pull/2526)을 참고했습니다.
이스케이프와 서명된 미디어 URL을 포함한 최종 JSON은 3,000바이트 이하로
제한합니다. 긴 캡션은 줄이고, URL이 유난히 길면 미디어 개수를 줄이거나
기존 임베드를 사용합니다.

Open Graph와 Twitter Card 태그는 대체 표시용으로 유지합니다.
유효한 Component Embed를 제공할 때만 Discordbot 응답의 ActivityPub
탐색 링크를 대체하며, 다른 클라이언트와 웹 미리보기는 기존 메타데이터를
사용합니다. 예비 규격과 Discord 클라이언트 지원은 바뀔 수 있으므로,
규격 테스트 통과가 모든 Discord 클라이언트의 지원을 보장하지는 않습니다.

> [!NOTE]
> 비공개 게시물, 연령 제한 게시물, 미국에서 볼 수 없는 게시물은 지원하지 않습니다.

## 구조

```text
Discord / Telegram / 브라우저
  → Cloudflare (DNS, 정적 CDN, WAF, Turnstile, Redirect Rules)
  → Cloudflare Tunnel → cloudflared 컨테이너
  → Vultr VM 한 대의 Go 앱 컨테이너 (게이트웨이, 임베드, 정적 웹, 미디어 미리보기)
  → /data의 SQLite (모델 캐시·지표, 별도의 프록시 예산 장부)
```

- **엣지:** 브라우저의 페이지 이동은 WAF보다 먼저 실행되는 Cloudflare Redirect
  Rules가 Instagram으로 보냅니다. 임베드 경로에서는 검증되지 않은 클라이언트를
  챌린지하므로, origin의 임베드 경로에는 검증된 봇만 도달합니다(홈, 정적 파일,
  `/api/*`, `/offload/*`는 이 규칙에서 제외). Go 앱도 같은 신호
  (`Sec-Fetch-Mode: navigate` + `Sec-Fetch-Dest: document`)로 최소한의 대체
  리다이렉트를 합니다. `www.d.`와 `www.g.`는 `d.`·`g.`로 308 리다이렉트하는
  상태 없는 Cloudflare Worker가 처리합니다(2단계 하위 도메인은 Workers Custom
  Domain으로만 무료 인증서를 받을 수 있습니다).
- **게시물:** Instagram embed 페이지(직접 요청)를 먼저 쓰고, 이어서 로그인 없는
  GraphQL과 외부 helper(미국 가정용 프록시 경유), 마지막으로 oEmbed를 씁니다.
  각 소스는 정해진 시간차로 경쟁하며, 이기기 전에 결과를 검증합니다. 승자는
  메모리에 캐시되고, 프록시로 얻은 결과(GraphQL, helper, 최종 oEmbed)는
  SQLite에도 저장됩니다.
- **프로필:** `web_profile_info`(프록시) 다음 프로필 embed 페이지.
  **스토리:** 외부 helper만 사용합니다.
- **미디어:** `/offload/*` 링크는 HMAC 서명된 권한 링크(수명 14일)이며
  Instagram CDN으로 리다이렉트합니다. 홈 미리보기 이미지는 FFmpeg로
  WebP/AVIF로 줄여 보냅니다.
- **프록시 예산:** DataImpulse 트래픽은 `100,000,000,000 / UTC 월의 일수`
  바이트의 일일 한도를 가지며, 1 MiB 단위로 미리 확보하고 재시작 후에도
  유지됩니다.

## 배포

Vultr High Frequency 1 GB VM(`vhf-1c-1gb`, 뉴저지) 한 대에서 Docker Compose로
실행합니다. 앱은 1 CPU / 512 MiB, `cloudflared`는 128 MiB로 제한하며 호스트
포트는 공개하지 않습니다. 이미지는 VM 밖에서 빌드해 SSH로 적재하고, 이미지
태그와 앱 버전은 8자리 커밋 해시입니다(`pnpm run image:build`는 커밋하지 않은 변경이나
`origin/main`에 없는 커밋이면 빌드를 거부합니다). 브랜치·커밋·릴리스·롤백 절차 전체는
[CONTRIBUTING.md](CONTRIBUTING.md)에 있습니다.

```bash
git switch main && git pull --ff-only
pnpm run check
pnpm run image:build    # oginstagram:<해시> 출력
docker save oginstagram:<해시> | gzip -1 | ssh linuxuser@<vm> 'gunzip | sudo docker load'
# VM에서 /opt/oginstagram/.env의 OG_IMAGE=oginstagram:<해시>로 바꾼 뒤
sudo sh tools/deploy.sh # 재시작, healthcheck, 현재·직전 이미지만 보관
sudo sh tools/backup.sh # data/backups에 일관된 SQLite 스냅샷
```

서버 준비, 비밀값, Tunnel 호스트, 현재 적용된 WAF·Redirect 규칙, 캐시 정책,
백업과 복구는 [Vultr 배포·복구 가이드](docs/vultr-deployment.md)에 있습니다.
앱 프로세스와 프록시 예산 장부는 하나뿐이므로, 같은 `/data`에 앱 인스턴스를
두 개 띄우지 않습니다.

## 개발

Go 1.26, Node.js(`package.json`의 engines 참고), pnpm이 필요합니다. Obscura와
FFmpeg가 포함된 운영 이미지를 시험하려면 Docker가 필요합니다.

```bash
pnpm install --frozen-lockfile
cp .env.example .env   # OFFLOAD_SIGNING_KEYS 설정. PROXY_* 없이는 embed 페이지 직접 요청만 동작
pnpm run dev           # 프론트엔드를 빌드하고 Go 앱을 :8080에서 실행
pnpm run check         # lint, 타입 검사, 경로·미리보기 테스트, Go 테스트
```

`pnpm run dev`는 `DEVELOPMENT=true`, localhost 호스트, 로컬 데이터 디렉터리,
빈 `TRUSTED_PROXIES`를 강제합니다. `.env`에 값이 없으면 Cloudflare의 항상 통과하는
Turnstile 테스트 site key와 오늘 UTC 날짜를 예산 시작일로 씁니다. 프론트엔드를
고친 뒤에는 다시 시작합니다. `PROXY_*`가 없으면 GraphQL, oEmbed, 프로필 API,
외부 helper(따라서 스토리)를 쓸 수 없습니다. `go run`으로는 helper 키를 다시
수집할 수 없고(이미지의 Obscura와 harvester 필요), WebP/AVIF 미리보기에는
libwebp와 libaom-av1이 포함된 `ffmpeg`가 `PATH`에 있어야 합니다(이미지에 포함).

## 설정

운영 변수는 `.env.example`에 있습니다. `PORT`, `DATA_DIR`, `ASSETS_DIR`은
Compose가 설정하고, Tunnel 토큰은 `secrets/tunnel-token`에서 마운트합니다.
`.env`는 관리자만 읽을 수 있게 둡니다.

| 변수 | 용도 |
| --- | --- |
| `OG_IMAGE`, `CLOUDFLARED_IMAGE` | Compose가 실행할 이미지 태그 |
| `BASE_URL`, `ALLOWED_HOSTS` | 대표 홈 URL과 허용하는 공개 호스트 (`BASE_URL`의 호스트가 목록에 있어야 함) |
| `TRUSTED_PROXIES` | `cloudflared`의 정확한 주소. 그 외 peer는 모두 거부 |
| `DEVELOPMENT` | 운영에서는 `false`. `true`면 시작 시 검증이 느슨해짐 |
| `PROXY_USERNAME`, `PROXY_PASSWORD` | DataImpulse 자격 증명. 둘 다 넣거나 둘 다 비움. 운영에서는 필수 |
| `PROXY_BUDGET_START_DATE` | 이 배포가 프록시를 쓰기 시작할 수 있는 첫 UTC 날짜. 뒤로 돌리지 않음 |
| `OFFLOAD_SIGNING_KEYS` | `/offload` 링크용 JSON HMAC 키링 |
| `WORKERHUB_SIGN_KEY`, `WORKERHUB_SIGN_TS` | 외부 helper 서명 값 직접 지정. 함께 교체 |
| `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET_KEY` | 홈 미리보기 위젯과 Siteverify |
| `ADMIN_PURGE_TOKEN` | `POST /api/admin/purge`용 Bearer 토큰 (로컬 캐시 비우기) |

`OFFLOAD_SIGNING_KEYS`는 패딩 없는 base64url 32바이트 키를 씁니다. 새 링크는
`active` 키로 서명하고, `keys`의 다른 키도 검증에는 계속 쓰이므로 키를 교체한
뒤 14일 동안은 이전 키를 남겨 둡니다.

```json
{"active":"2026-10","keys":{"2026-10":"<키>","2026-07":"<이전 키>"}}
```

서명이 없거나 형식이 틀렸거나 만료된 `/offload` 링크는 캐시를 보기 전에 404를
반환합니다. 임베드, API, `/offload` 리다이렉트와 오류 응답은 `no-store`입니다.
홈은 `private, no-cache`이고, 홈 미리보기 이미지는 브라우저 캐시는 허용하되
`Cloudflare-CDN-Cache-Control: no-store`를 붙입니다. 그래서 Cloudflare는 정적
파일만 캐시합니다. 로그는 stderr로 나가는 JSON 줄이며 Docker가 회전합니다.

외부 helper와 PP Mori 폰트는 비공개입니다. 저장소의 `server/external_helper.go`는
공개용 대체 구현이고, 비공개 구현이 있으면 init에서 스스로 설치됩니다. PP Mori
파일이 없으면 Pretendard(jsDelivr에서 로드), 그다음 시스템 폰트로 대체됩니다.

## 미리보기 보호

홈 페이지는 토큰 없이 `POST /api/embed`를 보냅니다. Cloudflare가
`Cf-Mitigated: challenge`로 응답하면 Turnstile 위젯(managed pre-clearance)을
실행하고 토큰을 넣어 한 번 다시 요청합니다. 앱은 토큰을 Siteverify로 확인하고
action과 hostname이 요청 호스트와 일치하는지 검사합니다. WAF 규칙
`http.request.method eq "POST" and http.request.uri.path eq "/api/embed"`는
Managed Challenge로 유지해야 합니다. 앱은 `cf_clearance` 쿠키가 하나 있을 것을
요구하지만 이를 통과 증명으로 쓰지는 않으며, 그 SHA-256 해시는 분당 8회
미리보기 요청 제한(429)의 키로만 씁니다.

## 감사의 말

- [FxEmbed/FxEmbed](https://github.com/FxEmbed/FxEmbed)
- [subzeroid/instagrapi](https://github.com/subzeroid/instagrapi)
- [Wikidepia/InstaFix](https://github.com/Wikidepia/InstaFix)
