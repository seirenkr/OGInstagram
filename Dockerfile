# Base images are pinned by digest (Docker build best practices); bump tag and
# digest together.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
ARG TARGETARCH
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/server . \
    && mkdir /out/data

# Build the frontend away from the 1 GB production VM.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c AS web
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY web/ web/
COPY shared/ shared/
COPY tools/build-home.mjs tools/build-home.mjs
RUN pnpm run build

# The server is one static binary: no shell, package manager, or root user.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS app
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/server /app/server
COPY --from=web /src/web/dist /app/web
ARG OG_VERSION=dev
ARG OG_REVISION=unknown
# OCI image annotations: https://github.com/opencontainers/image-spec/blob/main/annotations.md
LABEL org.opencontainers.image.title=oginstagram \
      org.opencontainers.image.version=$OG_VERSION \
      org.opencontainers.image.revision=$OG_REVISION \
      org.opencontainers.image.source=https://github.com/seirenkr/OGInstagram
ENV PORT=8080 DATA_DIR=/data ASSETS_DIR=/app/web OG_VERSION=$OG_VERSION
WORKDIR /app
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/server", "--healthcheck"]
ENTRYPOINT ["/app/server"]
