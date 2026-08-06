# Multi-stage build producing the default minimal engine image plus an optional
# fixer-runtime target with git, OpenCode, and pinned local srt sandboxing. Both
# targets include the server, fetcher, worker, and SPA.

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
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/server ./cmd/server

# Optional full engine image for local sandboxed OpenCode fix generation.
FROM node:20-slim AS fixer-runtime
ARG OPENCODE_VERSION=1.18.2
COPY hack/install-srt.sh /usr/local/bin/install-srt
RUN apt-get update \
 && apt-get install -y --no-install-recommends bash bubblewrap ca-certificates curl git ripgrep socat \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g "opencode-ai@${OPENCODE_VERSION}" \
 && install-srt /usr/local/share/prow-ai-dashboard/srt \
 && ln -s /usr/local/share/prow-ai-dashboard/srt/node_modules/.bin/srt /usr/local/bin/srt \
 && opencode --version \
 && node -e "if (require('/usr/local/share/prow-ai-dashboard/srt/node_modules/@anthropic-ai/sandbox-runtime/package.json').version !== '0.0.70') process.exit(1)"
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=web /src/frontend/dist /app/web
ENV HOME=/tmp \
    SRT_BIN=/usr/local/bin/srt
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
