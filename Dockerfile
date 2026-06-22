FROM node:24-alpine AS web
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY web/ web/
COPY tsconfig.json ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/server .

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=web /src/web/dist/ /app/assets/
COPY web/index.html /app/assets/index.html
ENV ASSETS_DIR=/app/assets
EXPOSE 8080
ENTRYPOINT ["/app/server"]
