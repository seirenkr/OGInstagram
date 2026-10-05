# Obscura headless V8 (multi-arch: amd64/arm64) — bundled for in-container
# HMAC key re-harvesting on external-helper signature rotation.
FROM h4ckf0r0day/obscura@sha256:16c2131acc625cd9ee4f8b95215afa3478ac391b68cb27b584ed841c7267343e AS obscura

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETARCH
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/server .

# Build the frontend away from the 1 GB production VM.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS web
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY harvest/package.json harvest/
RUN corepack enable && pnpm install --frozen-lockfile
COPY web/ web/
COPY shared/ shared/
COPY tsconfig.json ./
COPY tools/build-home.mjs tools/build-home.mjs
RUN pnpm run build

# playwright-core connects to the bundled Obscura over CDP.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS harvest
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY harvest/package.json harvest/
RUN corepack enable && pnpm --dir harvest install --prod --frozen-lockfile \
    --lockfile-dir=. --virtual-store-dir=harvest/node_modules/.pnpm

# Real negotiated WebP/AVIF previews need ffmpeg; Obscura needs glibc.
FROM node:22-bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg \
    && ffmpeg -hide_banner -encoders 2>/dev/null | grep -q 'libwebp ' \
    && ffmpeg -hide_banner -encoders 2>/dev/null | grep -q 'libaom-av1 ' \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /nodejs/bin /app /data \
    && ln -s /usr/local/bin/node /nodejs/bin/node \
    && chown 65532:65532 /data
COPY --from=obscura /obscura /app/obscura
COPY --from=build /out/server /app/server
# playwright-core has no dependencies; the workspace install also pulls frontend packages.
COPY --from=harvest /src/harvest/node_modules/playwright-core /app/harvest/node_modules/playwright-core
COPY harvest/harvest.mjs /app/harvest/
COPY --from=web /src/web/dist /app/web
# Declared late so a new version does not invalidate the ffmpeg layer.
ARG OG_VERSION=dev
ENV PORT=8080 DATA_DIR=/data ASSETS_DIR=/app/web OG_VERSION=$OG_VERSION
WORKDIR /app
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/server", "--healthcheck"]
ENTRYPOINT ["/app/server"]
