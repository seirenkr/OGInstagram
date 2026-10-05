# Vultr High Frequency 단일 서버 배포

대상은 **Vultr High Frequency `vhf-1c-1gb`, New Jersey(`ewr`), Ubuntu 24.04 LTS**입니다. 사양은 공유 1 vCPU·RAM 1,024 MB·32 GB NVMe·월 전송량 1,024 GB이고 **$6/월 세전, 한국 개인 VAT 10% 적용 시 예상 $6.60/월**입니다. provider 자동백업은 기본으로 선택하지 않습니다. 선택하면 +20%로 $7.20 세전·예상 $7.92 세후입니다. 환전 비용·초과 전송비는 별도입니다. [공식 계획 API](https://api.vultr.com/v2/plans), [한국 VAT 안내](https://docs.vultr.com/support/platform/billing/does-vultr-collect-vat-or-sales-tax), [자동백업 정책](https://docs.vultr.com/vps-automatic-backups)

모든 애플리케이션 코드와 상태는 이 VM에서 실행하고 Cloudflare DNS/CDN/WAF/Turnstile은 유지합니다. 기존 Worker를 중지하고 새 앱을 배포하는 방식이며 서비스 중단 시간을 허용합니다. 앱은 **512 MiB·1 CPU**, connector는 **128 MiB** 제한을 유지하며 서버 추가·자동 확장·유료 추가 옵션을 구성하지 않습니다. High Frequency는 일반형보다 $1 높은 가격에 3 GHz 초과 CPU와 NVMe를 제공하며 사용자의 선호에 따라 선택했습니다. 이 앱에서 High Performance AMD보다 빠른지는 실측하지 않았습니다. [Vultr 상품 구분](https://docs.vultr.com/products/compute/instances/cloud-compute/provisioning)

```text
인터넷 → Cloudflare DNS / CDN / WAF → Cloudflare Tunnel
                                    ↓
                   cloudflared 네트워크 connector (172.30.0.3, 128 MiB)
                                    ↓
                   Go + 웹 + harvester (512 MiB)
                                    ↓
                 /data/state.sqlite + budget.sqlite
```

Compose는 앱 포트를 호스트에 공개하지 않습니다. cloudflared가 Compose 네트워크의 `app:8080`으로 연결하고, Readiness는 컨테이너 내부의 CLI로 확인합니다. 인터넷에서 접근하는 경로는 Tunnel 하나입니다. `cloudflared`는 네트워크 연결만 담당하며 앱·DB·캐시는 모두 VM의 Go와 SQLite에서 실행합니다. Instagram embed 페이지와 helper 키 re-harvest(Obscura)는 Vultr의 인터넷 연결로 직접 나가고, logged-out GraphQL·oEmbed·web_profile_info·external helper(fastdl) API는 DataImpulse US residential proxy를 사용하며 일일 proxy budget에 포함됩니다. fastdl 앞단 Cloudflare가 데이터센터 IP를 rate limit(1015)하기 때문입니다.

## 1. 서버와 Docker 준비

사용자가 직접 Vultr 계정을 만들고 이메일과 결제 수단을 확인합니다. 신용카드 등록 화면의 **$0.00 deposit** 옵션을 선택하면 최초 충전 없이 카드를 연결할 수 있습니다. 카드사의 일시적인 소액 승인 보류는 발생할 수 있고, 다른 결제 수단에는 deposit이 필요할 수 있습니다. [계정 생성 공식 안내](https://docs.vultr.com/platform/create-an-account)

Vultr는 서버를 시간 단위로 과금하고 익월 1일에 전월 사용액의 청구서를 만듭니다. Cloud Compute의 월 과금 상한은 672시간입니다. 서버를 정지해도 자원이 남아 있으면 과금하며, 종료하려면 instance를 삭제해야 합니다. 장기 선결제 상품은 사용하지 않습니다. [공식 청구 방식](https://docs.vultr.com/support/platform/billing/how-am-i-billed-for-my-servers)

서버 생성 시 Shared CPU → High Frequency → New Jersey → `vhf-1c-1gb` → Ubuntu 24.04 LTS를 선택하고 SSH 키와 Firewall Group을 지정합니다. 자동백업·유료 DDoS 옵션·snapshot·추가 디스크/IP·유료 관리 패널은 기본 구성에서 선택하지 않습니다. **Limited User Login**을 사용하면 기본 sudo 계정은 `linuxuser`이며, 미선택 시 root 접속 뒤 sudo 가능한 관리 계정을 준비합니다. `ubuntu` 계정이 있다고 가정하지 말고 Console의 실제 접속 정보를 확인합니다. [서버 생성과 사용자 옵션](https://docs.vultr.com/products/compute/instances/cloud-compute/provisioning), [sudo 사용자 안내](https://docs.vultr.com/how-to-create-a-sudo-user-in-linux)

Vultr Firewall Group과 Ubuntu 방화벽은 IPv4·IPv6의 인터넷 incoming을 기본 거부하고 관리자의 IP에서 SSH만 허용합니다. 필요한 ICMP/ICMPv6는 유지합니다. SSH 허용 규칙을 먼저 준비하고 새 연결 성공을 확인한 뒤 제한합니다. 앱의 80·443·8080은 인터넷에 열지 않습니다. Outgoing과 established 응답은 허용하여 HTTPS·DNS·DataImpulse TCP 823·Tunnel UDP/TCP 7844를 차단하지 않습니다. [Vultr 네트워크 방화벽](https://docs.vultr.com/products/network/firewall-groups/management/rules), [Vultr UFW 안내](https://docs.vultr.com/how-to-configure-uncomplicated-firewall-ufw-on-ubuntu-20-04)

`cloudflared`는 기본 `auto` 프로토콜을 사용합니다. QUIC를 우선하고 UDP 연결이 불가능하면 HTTP/2로 전환하므로 outbound **UDP와 TCP 7844**를 모두 허용합니다. [Tunnel 프로토콜](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/run-parameters/), [Tunnel 방화벽 요건](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/tunnel-with-firewall/)

이후 서버 명령은 관리 계정으로 SSH 접속하여 실행합니다. Docker Engine과 Compose plugin은 [Vultr의 Ubuntu 24.04 안내](https://docs.vultr.com/how-to-install-docker-on-ubuntu-24-04)와 [Docker 공식 Ubuntu 설치 절차](https://docs.docker.com/engine/install/ubuntu/)에 맞춰 설치합니다.

```bash
sudo apt update
sudo apt install -y ca-certificates curl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
sudo tee /etc/apt/sources.list.d/docker.sources >/dev/null <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: $(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
EOF
sudo apt update
sudo apt install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo systemctl enable --now docker
sudo docker run --rm hello-world
sudo docker compose version
```

Compose는 `docker compose` plugin을 사용합니다. [Docker 공식 Compose 설치 안내](https://docs.docker.com/compose/install/linux/)와 같은 방식이며 Python/pip의 기존 `docker-compose`는 필요하지 않습니다. Docker가 공개한 포트는 UFW 정책을 우회할 수 있으므로 `compose.yaml`에 `ports:`를 추가하지 않습니다.

## 2. 이미지는 다른 컴퓨터에서 빌드

개발 컴퓨터에서 검사하고 서버 아키텍처(`linux/amd64`)용 이미지를 빌드한 뒤, 레지스트리 없이 SSH로 VM에 바로 적재합니다. 운영 VM에서는 빌드하지 않습니다. 이미지 태그와 앱 버전(`OG_VERSION`)은 같은 8자리 커밋 해시입니다. 커밋하지 않은 변경이나 `origin/main`에 없는 커밋이면 `pnpm run image:build`가 빌드를 거부하므로, PR을 `main`에 병합한 뒤 `main`에서 빌드합니다.

```bash
pnpm install --frozen-lockfile
pnpm run check
pnpm run image:build   # 마지막 줄에 oginstagram:<8자리 해시> 출력
docker save oginstagram:<해시> | gzip -1 | ssh linuxuser@<VM> 'gunzip | sudo docker load'
```

이 이미지는 Go 서버, 빌드된 `web/dist`, Obscura, Node harvester, WebP/AVIF encoder가 있는 FFmpeg를 포함합니다. 공개 checkout에는 비공개 external helper(`server/external_helper_private.go`)와 PP Mori 폰트(`web/public/PPMori-*.woff2`)가 없으므로 운영과 동일한 이미지를 만들려면 두 파일을 포함한 checkout에서 빌드합니다. 빌드 컨텍스트에서 `.env`, `data`, `secrets`는 제외됩니다. cloudflared는 검증한 tag/digest를 고정하며, `.env.example`의 `2026.9.3`은 [공식 릴리스](https://github.com/cloudflare/cloudflared/releases/tag/2026.9.3) 기준 기본값입니다.

## 3. 배포 디렉터리와 비밀 값

VM에는 `compose.yaml`, `.env.example`, `tools/deploy.sh`, `tools/backup.sh`만 복사해도 실행할 수 있습니다. 아래 명령은 `/opt/oginstagram`에서 수행합니다.

```bash
sudo install -d -m 0750 /opt/oginstagram
# 위 파일들을 /opt/oginstagram으로 전송한 뒤:
cd /opt/oginstagram
sudo cp .env.example .env
sudo chmod 0600 .env
sudo install -d -m 0700 -o 65532 -g 65532 data
sudo install -d -m 0700 secrets
sudo install -m 0600 /dev/null secrets/tunnel-token
sudoedit .env secrets/tunnel-token
sudo chown 65532:65532 secrets/tunnel-token
sudo chmod 0400 secrets/tunnel-token
```

`.env`에는 `.env.example`의 네 값(proxy credentials, Turnstile keys)과 `OG_IMAGE`만 넣습니다. 비밀번호에 `$` 등이 있으면 작은따옴표로 감싸 Compose 보간을 피합니다. 나머지는 기본값을 씁니다.

- `TRUSTED_PROXIES`, `CLOUDFLARED_IMAGE`, `DATA_DIR`, `ASSETS_DIR`: `compose.yaml`이 고정합니다. subnet을 바꾸면 Compose의 connector 주소와 `TRUSTED_PROXIES`를 함께 바꿉니다.
- `BASE_URL`, `ALLOWED_HOSTS`: `https://oginstagram.com`과 그 6개 hostname이 기본값입니다.
- `OFFLOAD_SIGNING_KEYS`: 비워 두면 첫 시작 때 `data/offload-signing-keys.json`(0600)을 만들어 계속 씁니다. 이 파일을 잃으면 이미 공유된 미디어 링크가 404가 되므로 백업에 포함합니다. 키를 교체할 때만 `OFFLOAD_SIGNING_KEYS='{"active":"new","keys":{"new":"…","old":"…"}}'`로 지정하고, 이전 키는 14일 뒤 제거합니다.
- `PROXY_BUDGET_START_DATE`: 기본값은 시작한 날(UTC)입니다. 같은 proxy 계정을 쓰던 다른 배포에서 넘어올 때만 그 배포를 멈춘 뒤의 **다음 UTC 날짜**로 지정합니다. 이 날짜는 앞으로만 움직입니다.
- `ADMIN_PURGE_TOKEN`: 설정하면 `POST /api/admin/purge`가 켜지고, 비우면 꺼집니다.
- `DISCORD_*_EMOJI_ID`: 기본값을 덮어쓸 때만 지정합니다.

터널 token은 `.env`나 command argument 값으로 넣지 않습니다. Compose secret file만 connector에 mount하고 `--token-file /run/secrets/tunnel_token`으로 읽습니다. [Cloudflare 문서](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/run-parameters/)의 remotely managed tunnel용 token-file 옵션은 2025.4.0 이상에서 지원하며, [Compose secret](https://docs.docker.com/compose/how-tos/use-secrets/)은 파일로 mount됩니다. 이 token으로 앱 secret이나 Cloudflare API token을 대체하지 않습니다.

## 4. Cloudflare 설정

[Cloudflare Tunnel 생성 안내](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/)에 따라 remotely managed tunnel을 만들고 token을 위 파일에 저장합니다. Published application 4개를 `HTTP`, 서비스 `app:8080`으로 연결합니다. HTTP Host header override는 비워 원래 public Host가 전달되게 합니다.

| Public hostname | Origin |
| --- | --- |
| `oginstagram.com` | `http://app:8080` |
| `www.oginstagram.com` | `http://app:8080` |
| `d.oginstagram.com` | `http://app:8080` |
| `g.oginstagram.com` | `http://app:8080` |
| `www.d.oginstagram.com` | 리다이렉트 Worker → `https://d.oginstagram.com` |
| `www.g.oginstagram.com` | 리다이렉트 Worker → `https://g.oginstagram.com` |

DNS는 [Tunnel의 DNS 안내](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/dns/)에 따라 proxied tunnel CNAME으로 연결합니다. 현재 Worker custom domain/route와 겹치는 레코드는 중단 시간에 교체합니다. 기존 Worker가 public hostname 요청을 계속 가로채면 새 origin으로 도달하지 않습니다. Public embed host에는 Access login을 추가하지 않습니다.

기존 Turnstile widget의 hostname `oginstagram.com`, managed pre-clearance, 아래 WAF **Managed Challenge** 규칙을 유지합니다.

```text
http.request.method eq "POST" and http.request.uri.path eq "/api/embed"
```

`cf_clearance`는 Cloudflare가 먼저 검증합니다. 앱은 이 cookie의 존재를 자체 인증으로 취급하지 않고, 보호된 요청의 rate-limit 식별자로만 사용합니다. token이 제공되면 앱도 Siteverify를 수행합니다. [Turnstile pre-clearance](https://developers.cloudflare.com/turnstile/get-started/pre-clearance/)와 [Siteverify](https://developers.cloudflare.com/turnstile/get-started/server-side-validation/)를 참고합니다.

`www.d.`와 `www.g.`는 2단계 하위 도메인이라 Universal SSL(`*.oginstagram.com`)이 덮지 않습니다. 무료 인증서는 Workers Custom Domain에만 발급되고, Custom Domain을 해제하면 그 인증서도 함께 삭제됩니다. 그래서 두 호스트는 상태 없는 리다이렉트 Worker `oginstagram-www-redirect`를 Custom Domain으로 연결해 첫 단계 호스트로 308을 보냅니다(ACM 유료 상품 불필요). WAF 규칙은 Worker보다 먼저 실행되므로 봇 판정은 동일합니다.

```js
export default {
  fetch(request) {
    const url = new URL(request.url);
    url.hostname = url.hostname.replace(/^www\./, "");
    return Response.redirect(url.toString(), 308);
  },
};
```

### 캐시

Cache Rule은 만들지 않습니다. Cloudflare 기본 동작은 확장자가 있는 정적 파일만 캐시하고, origin의 `Cache-Control`을 따릅니다. Go가 캐시 정책의 유일한 소유자입니다.

| 응답 | Go의 `Cache-Control` |
| --- | --- |
| 해시된 `/assets/*`, `/preview/*`, `/twemoji/*`, PP Mori | `public, max-age=31536000, immutable` |
| favicon, 기본 아바타 | `public, max-age=3600` |
| 홈 `/` (CSP nonce·언어 쿠키) | `private, no-cache` |
| 임베드·API·`/offload/*` 리다이렉트·오류 | `no-store` (서명 검증과 봇/사람 분기가 매 요청 origin에서 실행) |
| 홈 미리보기용 same-origin `/offload/*?preview=1\|avatar` 이미지 | `public, max-age=` 최대 3600(서명 `exp`까지) + `Vary: Accept`, `Cloudflare-CDN-Cache-Control: no-store` (브라우저만 캐시하고 엣지는 매번 origin 검증) |

Caching → Browser Cache TTL은 **Respect Existing Headers**로 둡니다. 다른 값은 Go의 짧은 TTL을 덮어씁니다.

### WAF와 Redirect 규칙 (Free 플랜, zone 전체)

두 규칙 묶음은 함께 동작합니다. Single Redirect가 WAF 커스텀 규칙보다 **먼저** 실행되므로, 사람의 클릭은 챌린지 없이 Instagram으로 이동하고 검증된 봇만 Go에 도달합니다. Redirect 규칙을 지우면 사람이 챌린지 화면을 먼저 보게 됩니다.

WAF 커스텀 규칙 1 "Unverified bot challenge" (Managed Challenge). 정적 파일 목록은 `server/home.go`의 `serveAsset` 허용 목록과 맞춥니다.

```text
http.request.uri.path ne "/"
and not starts_with(http.request.uri.path, "/assets/")
and not starts_with(http.request.uri.path, "/preview/")
and not starts_with(http.request.uri.path, "/twemoji/")
and not http.request.uri.path in {"/favicon-64.png" "/favicon-192.png" "/default-avatar.jpg" "/PPMori-Regular.woff2" "/PPMori-Semibold.woff2" "/robots.txt" "/favicon.ico" "/sitemap.xml"}
and not http.request.uri.path in {"/api/embed" "/api/status" "/api/admin/purge"}
and not starts_with(http.request.uri.path, "/offload/")
and not starts_with(http.request.uri.path, "/cdn-cgi/")
and not cf.client.bot
and not (http.user_agent contains "Discordbot/2.0" and ip.src.asnum in {15169 396982})
and not (http.user_agent contains "Blueno" and ip.src.asnum eq 23576)
```

WAF 커스텀 규칙 2 "Preview API challenge" (Managed Challenge): `http.request.method eq "POST" and http.request.uri.path eq "/api/embed"`. 검증되지 않은 연합우주 서버의 ActivityPub/WebFinger 요청도 규칙 1에서 챌린지됩니다.

Single Redirect 5개는 모두 호스트 `oginstagram.com`, `www.`, `g.`, `www.g.`와 GET/HEAD, `Sec-Fetch-Mode: navigate`, `Sec-Fetch-Dest: document`를 조건으로 307을 보냅니다.

| 규칙 | 경로 | 대상 |
| --- | --- | --- |
| 게시물 미디어 | `/p/C/N`, `/u/p/C/N` (끝 `/` 없음) | `https://www.instagram.com/p/C/?img_index=N` |
| 릴스 미디어 | `/reel(s)/C/N`, `/u/reel(s)/C/N` | `https://www.instagram.com/reel/C/?img_index=N` |
| 게시물 | `/p/C[/]`, `/u/p/C[/]` (쿼리 유지) | `https://www.instagram.com/p/C[/]` |
| 릴스 | `/reel(s)/C[/]`, `/u/reel(s)/C[/]` (쿼리 유지) | `https://www.instagram.com/reel/C[/]` |
| 스토리·프로필 | `/stories/u/ID[/]`, 1단계 경로(31자 이하) | `https://www.instagram.com` + 경로 |

Go(`gateway.go`)도 같은 기준(`Sec-Fetch-Mode`/`Sec-Fetch-Dest`)으로 사람을 판정해 규칙이 다루지 않는 형태(예: `/p/C/2/`, 챌린지를 통과한 방문자)를 Instagram으로 보냅니다. 두 계층은 같은 Instagram 리소스로 보내면 되며 끝 슬래시·쿼리 차이는 허용합니다. Go에 새 사람용 경로 형태를 추가하면 Redirect 규칙도 함께 갱신합니다. Fetch Metadata 헤더를 보내지 않는 오래된 브라우저(Safari·iOS 16.4 미만)는 두 계층 모두 리다이렉트하지 않으며, 메타 태그만 있는 임베드 HTML을 받습니다. 알고 받아들인 한계입니다.

## 5. 중지 후 새 앱 배포

1. 위 파일, `docker load`로 적재한 앱 이미지, Tunnel·WAF·Redirect 설정을 준비합니다. 다음 UTC 예산 시작일을 확인합니다.
2. 기존 Worker의 6개 custom domain/route를 중지하고 새 요청이 old Container로 들어가지 않는지 확인합니다. in-flight 요청 종료를 기다립니다. 신규 app과 old app이 각각 독립된 예산으로 같은 UTC 날짜에 proxy를 소비하게 하지 않습니다.
3. 필요하면 zone cache를 purge합니다. 기존 앱 Worker의 Custom Domain을 해제하고 `www.d.`/`www.g.`는 리다이렉트 Worker에 연결합니다. 기존 Worker 스크립트·Container·KV·DO·Analytics Engine 데이터 삭제는 영구 삭제이므로 관리자가 직접 판단합니다.
4. 먼저 신규 app만 시작해 readiness를 확인합니다.

```bash
cd /opt/oginstagram
sudo docker compose config --quiet
sudo docker compose pull cloudflared   # app 이미지는 docker load로 적재(레지스트리 없음)
sudo docker compose up -d --wait --wait-timeout 90 app
```

5. 첫 단계 호스트 4개를 Tunnel published application/DNS로 교체하고 connector를 시작합니다.

```bash
sudo docker compose up -d --wait --wait-timeout 90 cloudflared
sudo docker compose ps
sudo docker compose logs --tail 50 app cloudflared
```

6. 브라우저에서 홈/언어 선택, challenge→preview, 여러 번의 clearance 재사용과 429를 확인합니다. post/reel/profile/story, `d.` direct, `g.` gallery 링크를 실제 Discord·Telegram 등 검증된 unfurler에 붙여 넣어 미리보기를 확인하고, 새로 발급한 서명 링크·변조 링크·만료 링크도 검사합니다. WAF 규칙 1 때문에 운영자 IP에서 bot UA로 보낸 curl은 challenge(403)를 받으며, Go는 UA가 아니라 `Sec-Fetch-Mode`/`Sec-Fetch-Dest`로 사람을 판정합니다. 운영 예산 시작일 이전에는 proxy fetch가 제한되는 것이 정상입니다. cache 상태 페이지는 신규 로컬 지표부터 채워지며 old Analytics Engine history는 이관하지 않습니다.

네트워크 방화벽으로 필요한 outbound HTTPS/DNS·DataImpulse TCP 823·Tunnel UDP/TCP 7844 연결을 차단하지 않습니다. 실제 Vultr IP의 Instagram 직접 요청 성공률은 운영 샘플로 확인해야 합니다. 로컬 이미지 검증은 Vultr egress 성공률 검증을 대신하지 않습니다.

## 6. 업데이트와 백업

새 이미지를 적재하고 `.env`의 `OG_IMAGE`를 그 태그로 바꾼 뒤 `sudo sh tools/deploy.sh`를 실행합니다. 스크립트는 cloudflared만 pull하고, connector와 app을 멈춘 뒤 단일 instance로 재시작하고, healthcheck 후 현재·직전 앱 이미지만 남기고 나머지를 정리합니다. 롤백은 출력된 직전 이미지로 `OG_IMAGE`를 되돌리고 스크립트를 다시 실행합니다. `/data`를 지우거나 새로 만들지 않습니다. 중단 후 최종 이미지를 배포하는 흐름을 유지하며 old Worker를 되살려 별도 budget으로 서비스하지 않습니다.

```bash
sudo sh tools/backup.sh
sudo docker compose stats --no-stream
df -h /opt/oginstagram
```

Vultr 유료 자동백업은 기본으로 선택하지 않습니다. 선택할 경우 월요금에 20%가 추가되고 최근 2개 서버 백업을 보관합니다. [Vultr 자동백업](https://docs.vultr.com/vps-automatic-backups)

앱의 Backup CLI는 존재하지 않는 대상 디렉터리에 SQLite `VACUUM INTO`로 consistent snapshot을 만들며 `state.sqlite`와 `budget.sqlite`, 그리고 자동 생성된 `offload-signing-keys.json`을 보관합니다. 실행 중인 WAL 파일을 단순히 `cp`하는 방식 대신 이 명령을 사용합니다. 완료된 backup 디렉터리와 `.env`, Tunnel token을 **암호화하여 VM 외부 보관처로** 복사하고 recovery 시간을 기록합니다. 새 유료 저장소를 자동으로 추가하지 않습니다. state DB의 모델/metrics는 제한된 캐시이며 budget DB는 비용 보호 상태이므로 서로 다른 중요도로 다룹니다. 오래된 snapshot의 budget 잔액을 그대로 재사용하지 않습니다. 외부 보관처 복사와 확인이 끝난 백업은 `sudo rm -rf data/backups/<stamp>`로 VM에서 지웁니다. `tools/deploy.sh`는 정상 배포 뒤 현재·직전 앱 이미지를 제외한 Docker 이미지를 정리합니다. 예전 템플릿에서 복사한 `.env`에 `OG_VERSION`이 있으면 이미지에 넣은 버전을 덮어쓰므로 지웁니다(`PORT`·`DATA_DIR`·`ASSETS_DIR`·`TURNSTILE_HOSTNAMES`·`CLOUDFLARE_*`도 더 이상 쓰지 않습니다).

## 7. 복구

복구할 백업을 `/opt/oginstagram/restore/`에 풀고 아래 명령을 실행합니다. 먼저 앱과 connector를 멈춥니다. 아래 명령은 현재 DB와 WAL/SHM을 교체하므로 반드시 정확한 디렉터리에서 수행합니다.

```bash
cd /opt/oginstagram
sudo docker compose stop cloudflared app
sudo rm -f data/state.sqlite data/state.sqlite-wal data/state.sqlite-shm \
  data/budget.sqlite data/budget.sqlite-wal data/budget.sqlite-shm
sudo install -o 65532 -g 65532 -m 0600 restore/state.sqlite data/state.sqlite
sudo install -o 65532 -g 65532 -m 0600 restore/budget.sqlite data/budget.sqlite
sudo docker compose run --rm --no-deps app --exhaust-budget
sudo docker compose up -d --wait --wait-timeout 90
```

`--exhaust-budget`가 성공하기 전에는 app을 시작하지 않습니다. 이 명령은 복구 당일 UTC quota를 모두 사용한 것으로 기록하여 snapshot 이후의 소비를 다시 지급하지 않게 합니다. proxy는 다음 UTC day부터 재개합니다. snapshot도 없거나 budget DB가 유실된 경우에도 `PROXY_BUDGET_START_DATE`를 다음 UTC 날짜로 설정하고 당일 quota를 재지급하지 않습니다. empty DB로 복구할 때는 state와 budget을 함께 새로 준비하며 기존 파일을 임의로 삭제해 quota를 초기화하지 않습니다.

## 운영 한계

1 vCPU·1,024 MB VM 한 대와 단일 SQLite writer를 전제로 합니다. 앱은 1 CPU·512 MiB, connector는 128 MiB로 제한하고 나머지는 OS/Docker에 남깁니다. FFmpeg는 변환 하나와 thread 하나로 제한합니다. harvester의 순간 메모리와 실제 처리량은 운영에서 관찰해야 합니다. Go memory limit은 Node/Obscura/FFmpeg까지 제한하지 않으며 Docker memory cap이 전체 프로세스를 제한합니다. Vultr 월 전송량 1,024 GB와 DataImpulse 월 100 GB는 별도입니다. Vultr는 outbound을 집계하며 초과요금은 $0.01/GB입니다. [전송량 집계](https://docs.vultr.com/support/platform/billing/how-is-bandwidth-usage-calculated), [초과요금](https://docs.vultr.com/support/platform/billing/what-is-the-bandwidth-overage-rate)

고가용성이나 여러 replica는 이 구성의 범위가 아닙니다. 같은 `/data`에 두 app을 붙이거나 별도 예산 DB를 가진 replica를 늘리지 않습니다. `Dockerfile`, `compose.yaml`, 예산 ledger 보존과 Cloudflare WAF·Redirect 규칙을 하나의 배포 단위로 관리합니다.

## 공식 Vultr MCP

[Vultr 공식 MCP](https://github.com/vultr/vultr-mcp)의 hosted endpoint는 `https://vultrmcp.com/mcp`이며 현재 조회 전용입니다. 서버 생성·변경은 로컬 STDIO 실행에서 `VULTR_API_KEY`와 `VULTR_MCP_WRITES_ENABLED=true`를 설정하고 계정의 API 접근 권한이 있어야 가능합니다. 이 플래그는 API 키 권한을 제한하는 경계가 아닙니다. Vultr API 키는 상품별 범위 제한을 지원하지 않으므로 MCP 실행 위치의 허용 IP를 제한하고 앱 컨테이너에는 전달하지 않습니다. [API 키 범위](https://docs.vultr.com/support/platform/api/can-i-scope-api-keys-to-specific-products), [API 접근 설정](https://docs.vultr.com/support/platform/users/how-can-i-manage-api-access-for-users)

MCP는 VM·방화벽·SSH 키 등 Vultr 리소스를 관리하고, 실제 앱 파일·secrets·Docker 배포는 SSH로 수행합니다. 계정 가입은 사용자가 직접 진행합니다. 이 문서 작업에서 MCP 설치·로그인·계정 및 클라우드 변경은 수행하지 않았습니다.
