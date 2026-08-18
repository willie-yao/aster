# Multi-stage build producing the default engine image plus specialized
# remote-fix, Agent Sandbox fix, analyzer, and causal-critic targets.

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
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/fetcher ./cmd/aster \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/fixexecutor ./cmd/fixexecutor \
 && CGO_ENABLED=0 go build -o /out/criticexecutor ./cmd/criticexecutor \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/analysisexecutor ./cmd/analysisexecutor \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/analysisstager ./cmd/analysisstager

# Pinned Go toolchain copied into the Agent Sandbox Fix executor.
FROM golang:1.25.12-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS agent-sandbox-fix-go

# Aster-pinned OpenCode analyzer runtime. The patch disables repository
# instruction discovery when project configuration is disabled.
FROM oven/bun:1.3.14-alpine@sha256:5acc90a93e91ff07bf72aa90a7c9f0fa189765aec90b47bdbf2152d2196383c0 AS opencode-analysis-build
ARG TARGETARCH
ARG OPENCODE_UPSTREAM_VERSION=1.18.2
ARG OPENCODE_UPSTREAM_REVISION=70b56a0a93d366889cae950379cc9d2537148fa2
ARG OPENCODE_SOURCE_ARCHIVE_SHA256=13d277b405def808734be8ce4c6f68d3b40df866358556aefb48b5be90ea53c1
ARG OPENCODE_PATCH_VERSION=aster-disable-project-instructions-v1
ARG OPENCODE_PATCH_SHA256=48031f5d9a3c675406c43697682291efba78feb208c9f5dc2a977645aa41e6a3
ARG OPENCODE_BUILD_PATCH_VERSION=aster-single-target-build-v1
ARG OPENCODE_BUILD_PATCH_SHA256=1d90634eebd407761327da845aa8cb3a72b18ea2dd33e6cd0f1904215db0b595
ARG OPENCODE_BUILDER_BUN_SHA256=500e6edbf321ddf490adcc55a6a01639993a07924616ab67492e1256c15557e2
ARG OPENCODE_MODELS_DEV_SHA256=2f6a5a4ab4d450e3ddabdbf0313e51bd76d51577ec1d7936326c484aded33b51
ARG OPENCODE_MODELS_DEV_GZIP_SHA256=3b34694d1de8b57bdd0cbcfae3b8376d8d9b56352a8655b9ab62a49ffbca785d
ARG OPENCODE_NPM_REGISTRY=https://registry.npmjs.org/
RUN apk add --no-cache ca-certificates curl git patch \
 && test "$(sha256sum /usr/local/bin/bun | cut -d' ' -f1)" = "${OPENCODE_BUILDER_BUN_SHA256}"
WORKDIR /src/opencode
RUN curl -fsSL --retry 3 \
      -o /tmp/opencode.tar.gz \
      "https://codeload.github.com/anomalyco/opencode/tar.gz/${OPENCODE_UPSTREAM_REVISION}" \
 && echo "${OPENCODE_SOURCE_ARCHIVE_SHA256}  /tmp/opencode.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/opencode.tar.gz --strip-components=1 \
 && rm /tmp/opencode.tar.gz \
 && test "$(bun --version)" = "1.3.14" \
 && test "$(git rev-parse HEAD 2>/dev/null || true)" = ""
COPY hack/patches/opencode-1.18.2-disable-project-instructions.patch /tmp/opencode.patch
COPY hack/patches/opencode-1.18.2-build-target.patch /tmp/opencode-build.patch
COPY hack/opencode/models-dev-api-2026-08-18.json.gz /tmp/models-dev-api.json.gz
RUN echo "${OPENCODE_PATCH_SHA256}  /tmp/opencode.patch" | sha256sum -c - \
 && echo "${OPENCODE_BUILD_PATCH_SHA256}  /tmp/opencode-build.patch" | sha256sum -c - \
 && echo "${OPENCODE_MODELS_DEV_GZIP_SHA256}  /tmp/models-dev-api.json.gz" | sha256sum -c - \
 && gzip -dc /tmp/models-dev-api.json.gz > /tmp/models-dev-api.json \
 && echo "${OPENCODE_MODELS_DEV_SHA256}  /tmp/models-dev-api.json" | sha256sum -c - \
 && patch -p1 --fuzz=0 < /tmp/opencode.patch \
 && patch -p1 --fuzz=0 < /tmp/opencode-build.patch \
 && grep -Fq 'if (Flag.OPENCODE_DISABLE_PROJECT_CONFIG) return []' packages/opencode/src/session/instruction.ts \
 && grep -Fq 'const requestedTarget = process.env.OPENCODE_BUILD_TARGET' packages/opencode/script/build.ts \
 && bun install --filter opencode --frozen-lockfile --ignore-scripts --cafile=/etc/ssl/certs/ca-certificates.crt --registry="${OPENCODE_NPM_REGISTRY}"
RUN test "${TARGETARCH}" = "amd64" \
 && OPENCODE_VERSION="${OPENCODE_UPSTREAM_VERSION}" OPENCODE_BUILD_TARGET=linux-x64-baseline-musl MODELS_DEV_API_JSON=/tmp/models-dev-api.json ./packages/opencode/script/build.ts --skip-install --skip-embed-web-ui \
 && install -D -m 0755 packages/opencode/dist/opencode-linux-x64-baseline-musl/bin/opencode /out/opencode \
 && test "$(/out/opencode --version)" = "${OPENCODE_UPSTREAM_VERSION}" \
 && binary_sha=$(sha256sum /out/opencode | cut -d' ' -f1) \
 && printf '%s\n' \
      '{' \
      '  "version": 1,' \
      "  \"upstream_version\": \"${OPENCODE_UPSTREAM_VERSION}\"," \
      "  \"upstream_revision\": \"${OPENCODE_UPSTREAM_REVISION}\"," \
      "  \"source_archive_sha256\": \"${OPENCODE_SOURCE_ARCHIVE_SHA256}\"," \
      "  \"models_dev_sha256\": \"${OPENCODE_MODELS_DEV_SHA256}\"," \
      '  "builder_image": "docker.io/oven/bun:1.3.14-alpine@sha256:5acc90a93e91ff07bf72aa90a7c9f0fa189765aec90b47bdbf2152d2196383c0",' \
      "  \"builder_bun_sha256\": \"${OPENCODE_BUILDER_BUN_SHA256}\"," \
      '  "bun_version": "1.3.14",' \
      '  "embedded_web_ui": false,' \
      "  \"patch_version\": \"${OPENCODE_PATCH_VERSION}\"," \
      "  \"patch_sha256\": \"${OPENCODE_PATCH_SHA256}\"," \
      "  \"build_patch_version\": \"${OPENCODE_BUILD_PATCH_VERSION}\"," \
      "  \"build_patch_sha256\": \"${OPENCODE_BUILD_PATCH_SHA256}\"," \
      "  \"binary_sha256\": \"${binary_sha}\"" \
      '}' > /out/opencode-runtime.json

# OpenCode executor for consumer-installed Agent Sandbox.
# OpenCode is inherited from its official image pinned by release and OCI digest.
FROM ghcr.io/anomalyco/opencode:1.18.2@sha256:ef9257b3246e9be63d5050924c07f7e6d8d9f135fdfcd8422fc873a408c367af AS agent-sandbox-fix-executor
ARG VERSION=dev
ARG COMMIT=dev
ARG IMAGE_TAG=dev
USER root
COPY --from=agent-sandbox-fix-go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH} \
    GOTOOLCHAIN=local
RUN apk add --no-cache ca-certificates git=2.54.0-r0 \
 && test "$(go env GOVERSION)" = "go1.25.12" \
 && test "$(git --version)" = "git version 2.54.0" \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && test "$(opencode --version)" = "1.18.2"
COPY --from=build /out/fixexecutor /usr/local/bin/fixexecutor
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster Agent Sandbox Fix Executor" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster" \
      org.opencontainers.image.version=${VERSION} \
      org.opencontainers.image.revision=${COMMIT} \
      io.prow-ai-dashboard.image-tag=${IMAGE_TAG}
ENV HOME=/tmp/home \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/fixexecutor"]

# File-backed OpenCode analyzer for a staged Agent Sandbox workspace.
FROM ghcr.io/anomalyco/opencode:1.18.2@sha256:ef9257b3246e9be63d5050924c07f7e6d8d9f135fdfcd8422fc873a408c367af AS agent-sandbox-analysis-executor
ARG VERSION=dev
ARG COMMIT=dev
ARG IMAGE_TAG=dev
ARG OPENCODE_UPSTREAM_VERSION=1.18.2
ARG OPENCODE_UPSTREAM_REVISION=70b56a0a93d366889cae950379cc9d2537148fa2
ARG OPENCODE_SOURCE_ARCHIVE_SHA256=13d277b405def808734be8ce4c6f68d3b40df866358556aefb48b5be90ea53c1
ARG OPENCODE_PATCH_VERSION=aster-disable-project-instructions-v1
ARG OPENCODE_PATCH_SHA256=48031f5d9a3c675406c43697682291efba78feb208c9f5dc2a977645aa41e6a3
ARG OPENCODE_BUILD_PATCH_VERSION=aster-single-target-build-v1
ARG OPENCODE_BUILD_PATCH_SHA256=1d90634eebd407761327da845aa8cb3a72b18ea2dd33e6cd0f1904215db0b595
ARG OPENCODE_BUILDER_BUN_SHA256=500e6edbf321ddf490adcc55a6a01639993a07924616ab67492e1256c15557e2
ARG OPENCODE_MODELS_DEV_SHA256=2f6a5a4ab4d450e3ddabdbf0313e51bd76d51577ec1d7936326c484aded33b51
USER root
RUN apk add --no-cache ca-certificates git \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot
COPY --from=opencode-analysis-build /out/opencode /usr/local/bin/opencode
COPY --from=opencode-analysis-build /out/opencode-runtime.json /usr/local/share/aster/opencode-runtime.json
RUN test "$(opencode --version)" = "${OPENCODE_UPSTREAM_VERSION}" \
 && test "$(sha256sum /usr/local/bin/opencode | cut -d' ' -f1)" = "$(sed -n 's/.*\"binary_sha256\": \"\([0-9a-f]*\)\".*/\1/p' /usr/local/share/aster/opencode-runtime.json)" \
 && git --version
COPY --from=build /out/analysisexecutor /usr/local/bin/analysisexecutor
ENV HOME=/tmp/home \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1 \
    GIT_OPTIONAL_LOCKS=0
USER 65532:65532
WORKDIR /workspace
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster Agent Sandbox Analysis Executor" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster" \
      org.opencontainers.image.version=${VERSION} \
      org.opencontainers.image.revision=${COMMIT} \
      io.prow-ai-dashboard.image-tag=${IMAGE_TAG} \
      io.aster.opencode.upstream.version=${OPENCODE_UPSTREAM_VERSION} \
      io.aster.opencode.upstream.revision=${OPENCODE_UPSTREAM_REVISION} \
      io.aster.opencode.source-archive.sha256=${OPENCODE_SOURCE_ARCHIVE_SHA256} \
      io.aster.opencode.models-dev.sha256=${OPENCODE_MODELS_DEV_SHA256} \
      io.aster.opencode.patch.version=${OPENCODE_PATCH_VERSION} \
      io.aster.opencode.patch.sha256=${OPENCODE_PATCH_SHA256} \
      io.aster.opencode.build-patch.version=${OPENCODE_BUILD_PATCH_VERSION} \
      io.aster.opencode.build-patch.sha256=${OPENCODE_BUILD_PATCH_SHA256} \
      io.aster.opencode.builder-bun.sha256=${OPENCODE_BUILDER_BUN_SHA256}
ENTRYPOINT ["/usr/local/bin/analysisexecutor"]

# Credential-free local snapshot copier for the analyzer init container.
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce AS agent-sandbox-analysis-stager
ARG VERSION=dev
ARG COMMIT=dev
ARG IMAGE_TAG=dev
RUN apk add --no-cache ca-certificates git \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && git --version
COPY --from=build /out/analysisstager /usr/local/bin/analysisstager
ENV HOME=/tmp/home \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster Agent Sandbox Analysis Stager" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster" \
      org.opencontainers.image.version=${VERSION} \
      org.opencontainers.image.revision=${COMMIT} \
      io.prow-ai-dashboard.image-tag=${IMAGE_TAG}
ENTRYPOINT ["/usr/local/bin/analysisstager"]

# Purpose-built credential-free causal critic for consumer-installed Agent Sandbox.
# It contains no shell, Git, coding-agent harness, package manager, or write tools.
FROM gcr.io/distroless/static-debian12:nonroot AS agent-sandbox-critic-executor
COPY --from=build /out/criticexecutor /usr/local/bin/criticexecutor
USER 65532:65532
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster Agent Sandbox Critic Executor" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster"
ENTRYPOINT ["/usr/local/bin/criticexecutor"]

# Minimal git-capable engine for reconstructing patches returned by remote fix
# runtimes such as Agent Sandbox. It intentionally omits any coding-agent harness.
FROM golang:1.25.12-alpine AS remote-fixer-runtime
RUN apk add --no-cache ca-certificates git=2.54.0-r0 \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && test "$(git --version)" = "git version 2.54.0"
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=web /src/frontend/dist /app/web
ENV HOME=/tmp \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster Remote Fixer" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster"
ENTRYPOINT ["/usr/local/bin/server"]

# Stage 3: minimal runtime. distroless/static ships CA certs for HTTPS to GCS,
# GitHub, and the AI endpoint, and runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=web /src/frontend/dist /app/web
USER 65532:65532
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster"
# Server is the default entrypoint; the fetcher CronJob overrides command.
ENTRYPOINT ["/usr/local/bin/server"]
