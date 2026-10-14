# Base images are pinned by digest (Docker build best practices); bump tag and
# digest together.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
ARG TARGETARCH
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/server .
# Scans with the toolchain that builds the release, so a reachable stdlib or
# module vulnerability fails the image build.
RUN go vet ./... && go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

# Build the frontend natively on the build platform; its output is arch-independent.
FROM --platform=$BUILDPLATFORM node:26-bookworm-slim@sha256:86f07bc9c5dce4578cf37e5a418b7bfc7f817cda25cde66e2b66e95ed86c4567 AS web
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN npm install -g corepack@0.36.0 && corepack enable && pnpm install --frozen-lockfile
COPY web/ web/
COPY shared/ shared/
COPY tools/build-home.mjs tools/build-home.mjs
RUN pnpm run build

# The server is one static binary: no shell, package manager, or root user.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS app
COPY --from=build /out/server /app/server
COPY --from=web /src/web/dist /app/web
ARG OG_VERSION=dev
ENV DATA_DIR=/data ASSETS_DIR=/app/web OG_VERSION=$OG_VERSION
USER 65532:65532
ENTRYPOINT ["/app/server"]
