# Multi-stage build producing the default engine image plus specialized local
# fixer, remote-fix, Agent Sandbox fix, and causal-critic targets.

# Stage 1: build the SPA. Default base path "/" suits server mode.
FROM node:20-alpine AS web
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: build the Go binaries.
FROM golang:1.25.12-bookworm AS build
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
ARG VERSION=dev
ARG COMMIT=dev
ARG IMAGE_TAG=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/fetcher ./cmd/fetcher \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -o /out/fixexecutor ./cmd/fixexecutor \
 && CGO_ENABLED=0 go build -o /out/criticexecutor ./cmd/criticexecutor \
 && CGO_ENABLED=0 go build -o /out/analysisexecutor ./cmd/analysisexecutor \
 && CGO_ENABLED=0 go build -o /out/analysisstager ./cmd/analysisstager

# Optional full engine image for local sandboxed OpenCode fix generation.
FROM node:20-slim AS fixer-runtime
ARG OPENCODE_VERSION=1.18.2
COPY hack/install-srt.sh /usr/local/bin/install-srt
COPY hack/build-srt-seccomp.mjs /usr/local/bin/build-srt-seccomp.mjs
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash bubblewrap ca-certificates curl gcc git libc6-dev libseccomp-dev ripgrep socat \
 && npm install -g "opencode-ai@${OPENCODE_VERSION}" \
 && install-srt /usr/local/share/prow-ai-dashboard/srt \
 && ln -s /usr/local/share/prow-ai-dashboard/srt/node_modules/.bin/srt /usr/local/bin/srt \
 && opencode --version \
 && node -e "if (require('/usr/local/share/prow-ai-dashboard/srt/node_modules/@anthropic-ai/sandbox-runtime/package.json').version !== '0.0.70') process.exit(1)" \
 && node -e "const fs=require('fs'); const arch={x64:'x64',arm64:'arm64'}[process.arch]; if (!arch || !fs.existsSync('/usr/local/share/prow-ai-dashboard/srt/node_modules/@anthropic-ai/sandbox-runtime/vendor/seccomp/'+arch+'/apply-seccomp')) process.exit(1)" \
 && apt-get purge -y --auto-remove curl gcc libc6-dev libseccomp-dev \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=web /src/frontend/dist /app/web
ENV HOME=/tmp \
    SRT_BIN=/usr/local/bin/srt
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/server"]

# OpenCode executor for consumer-installed Agent Sandbox.
# OpenCode is inherited from its official image pinned by release and OCI digest.
FROM ghcr.io/anomalyco/opencode:1.18.2@sha256:ef9257b3246e9be63d5050924c07f7e6d8d9f135fdfcd8422fc873a408c367af AS agent-sandbox-fix-executor
USER root
RUN apk add --no-cache ca-certificates git \
 && go version \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && opencode --version
COPY --from=build /out/fixexecutor /usr/local/bin/fixexecutor
ENV HOME=/tmp/home \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/fixexecutor"]

# File-backed OpenCode analyzer for a staged Agent Sandbox workspace.
FROM ghcr.io/anomalyco/opencode:1.18.2@sha256:ef9257b3246e9be63d5050924c07f7e6d8d9f135fdfcd8422fc873a408c367af AS agent-sandbox-analysis-executor
USER root
RUN apk add --no-cache ca-certificates git \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && opencode --version \
 && git --version
COPY --from=build /out/analysisexecutor /usr/local/bin/analysisexecutor
ENV HOME=/tmp/home \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_OPTIONAL_LOCKS=0
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/analysisexecutor"]

# Credential-free local snapshot copier for the analyzer init container.
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce AS agent-sandbox-analysis-stager
RUN apk add --no-cache ca-certificates git \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && git --version
COPY --from=build /out/analysisstager /usr/local/bin/analysisstager
ENV HOME=/tmp/home \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/analysisstager"]

# Purpose-built credential-free causal critic for consumer-installed Agent Sandbox.
# It contains no shell, Git, coding-agent harness, package manager, or write tools.
FROM gcr.io/distroless/static-debian12:nonroot AS agent-sandbox-critic-executor
COPY --from=build /out/criticexecutor /usr/local/bin/criticexecutor
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/criticexecutor"]

# Minimal git-capable engine for reconstructing patches returned by remote fix
# runtimes such as Agent Sandbox. It intentionally omits OpenCode and srt.
FROM golang:1.25.12-alpine AS remote-fixer-runtime
RUN apk add --no-cache ca-certificates git \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && git --version
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=web /src/frontend/dist /app/web
ENV HOME=/tmp \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/server"]

# Stage 3: minimal runtime. distroless/static ships CA certs for HTTPS to GCS,
# GitHub, and the AI endpoint, and runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=web /src/frontend/dist /app/web
USER 65532:65532
# Server is the default entrypoint; the fetcher CronJob overrides command.
ENTRYPOINT ["/usr/local/bin/server"]
