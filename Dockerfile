# Go plugins only load into a dynamically linked gateway built with the same
# toolchain, libc and dependency versions, so the gateway and the plugin are
# compiled side by side from one pinned Bifrost release.
ARG BIFROST_REF=transports/v2.5.1
ARG BIFROST_VERSION=2.5.1

FROM alpine:3.24 AS source
ARG BIFROST_REF
RUN apk add --no-cache git && git clone --depth 1 --branch "${BIFROST_REF}" https://github.com/maximhq/bifrost /src

FROM node:25-alpine3.23 AS ui
WORKDIR /app
COPY --from=source /src/ui/package*.json ./
RUN npm ci
COPY --from=source /src/ui/ ./
# Upstream also type-checks here; that only validates their source and needs
# more memory than a default Docker Desktop VM has.
RUN npx vite build

FROM golang:1.27.3-alpine3.24 AS build
ARG BIFROST_VERSION
RUN apk add --no-cache gcc musl-dev sqlite-dev
ENV CGO_ENABLED=1 GOOS=linux GOWORK=off

WORKDIR /gateway
COPY --from=source /src/transports/ ./
RUN go mod download
COPY --from=ui /app/out ./bifrost-http/ui
RUN go build -trimpath \
      -ldflags="-w -s -X main.Version=v${BIFROST_VERSION}" \
      -o /out/bifrost ./bifrost-http

WORKDIR /plugin
COPY . .
COPY scripts/align-deps.sh /usr/local/bin/align-deps
RUN --mount=type=cache,target=/root/.cache/go-build sh /usr/local/bin/align-deps /gateway /plugin
RUN --mount=type=cache,target=/root/.cache/go-build go test ./...
RUN --mount=type=cache,target=/root/.cache/go-build go build -buildmode=plugin -trimpath -o /out/bestbefore.so .

FROM alpine:3.24
RUN apk add --no-cache musl libgcc ca-certificates zlib && \
    adduser -D -s /bin/sh appuser && \
    mkdir -p /app/data /app/plugins && chown -R appuser /app
WORKDIR /app
# The upstream entrypoint runs /app/main.
COPY --from=build /out/bifrost /app/main
COPY --from=build /out/bestbefore.so /app/plugins/bestbefore.so
COPY --from=source /src/transports/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh
ENV APP_PORT=8080 APP_HOST=0.0.0.0 APP_DIR=/app/data LOG_LEVEL=info LOG_STYLE=pretty
USER appuser
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/app/docker-entrypoint.sh"]
