# Multi-stage build producing the default minimal engine image plus an optional
# fixer-runtime target with git. Both targets include the server,
# fetcher, worker, and SPA; the fixer target also includes orka-ingestor.

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
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/fetcher ./cmd/fetcher \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -o /out/orka-ingestor ./cmd/orka-ingestor

# Optional drop-in chart image with git for Orka diff reconstruction.
FROM debian:bookworm-slim AS fixer-runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=build /out/orka-ingestor /usr/local/bin/orka-ingestor
COPY --from=web /src/frontend/dist /app/web
ENV HOME=/tmp
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
