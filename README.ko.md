# OGInstagram

[English](README.md) | **한국어**

Discord, Telegram 등 링크 미리보기 봇을 위한 Instagram 임베드 프록시입니다.

## 사용법

게시물·릴스·프로필·스토리 링크의 `instagram.com`을 바꿉니다.

| 호스트 | 임베드 |
|--------|--------|
| `oginstagram.com` | 작성자, 캡션, 통계, 미디어 |
| `g.oginstagram.com` | 작성자, 미디어 |
| `d.oginstagram.com` | 미디어 파일만 |

캐러셀 항목은 `?img_index=N`으로 고릅니다. 비공개·연령 제한 게시물은 지원하지 않습니다.

## 실행

```bash
pnpm install --frozen-lockfile
pnpm run dev     # http://localhost:8080
pnpm run check   # lint, 타입, 테스트
```

운영 환경은 Cloudflare Tunnel 뒤에서 `compose.yaml`로 실행합니다(`secrets/tunnel-token`, UID 65532 소유의 `data/`). `.env.example`의 값만 채우면 되고 나머지는 기본값이 있습니다. 릴리스 절차는 [CONTRIBUTING.md](CONTRIBUTING.md)를 따릅니다.

## 라이선스

[LICENSE](LICENSE) · [서드파티 고지](THIRD_PARTY_NOTICES.md)
