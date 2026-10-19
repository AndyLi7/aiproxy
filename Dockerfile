FROM node:22-alpine AS frontend-builder

WORKDIR /aiproxy/web

COPY ./web/ ./

RUN corepack pnpm install --frozen-lockfile && corepack pnpm run build

FROM golang:1.27-alpine AS builder

ARG GOPROXY=https://proxy.golang.org,direct
ARG SWAG_VERSION=v1.16.6
ENV GOPROXY=${GOPROXY}

WORKDIR /aiproxy/core

RUN go install github.com/swaggo/swag/cmd/swag@${SWAG_VERSION}

COPY ./core/go.mod ./core/go.sum ./
COPY ./mcp-servers/go.mod ./mcp-servers/go.sum /aiproxy/mcp-servers/
COPY ./openapi-mcp/go.mod ./openapi-mcp/go.sum /aiproxy/openapi-mcp/

RUN go mod download

COPY ./core/ ./
COPY ./mcp-servers/ /aiproxy/mcp-servers/
COPY ./openapi-mcp/ /aiproxy/openapi-mcp/

COPY --from=frontend-builder /aiproxy/web/dist/ /aiproxy/core/public/dist/

RUN SWAG_VERSION=${SWAG_VERSION} sh scripts/swag.sh

# Contract tests run before every deploy (go test), not inside the image
# build: shared builders are slow enough to time out timing-sensitive tests.

RUN go build -trimpath -ldflags "-s -w" -o aiproxy

FROM alpine:latest

RUN addgroup -S -g 10001 aiproxy && \
    adduser -S -D -H -u 10001 -G aiproxy aiproxy && \
    mkdir -p /aiproxy && chown aiproxy:aiproxy /aiproxy

WORKDIR /aiproxy

RUN apk add --no-cache ca-certificates tzdata ffmpeg curl && \
    rm -rf /var/cache/apk/*

COPY --from=builder /aiproxy/core/aiproxy /usr/local/bin/aiproxy

ENV PUID=10001 PGID=10001 UMASK=022

# Existing bind mounts must permit UID/GID 10001 to write database, logs and
# generated local configuration. Provision volume permissions before upgrading;
# the application does not change ownership of existing data on startup.
USER 10001:10001

ENV FFMPEG_ENABLED=true

EXPOSE 3000

ENTRYPOINT ["aiproxy"]
