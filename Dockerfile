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

RUN go build -trimpath -ldflags "-s -w" -o aiproxy

FROM alpine:latest

RUN mkdir -p /aiproxy

WORKDIR /aiproxy

RUN apk add --no-cache ca-certificates tzdata ffmpeg curl && \
    rm -rf /var/cache/apk/*

COPY --from=builder /aiproxy/core/aiproxy /usr/local/bin/aiproxy

ENV PUID=0 PGID=0 UMASK=022

ENV FFMPEG_ENABLED=true

EXPOSE 3000

ENTRYPOINT ["aiproxy"]
