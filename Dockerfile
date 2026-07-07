# Multi-stage build producing one image with both the fetcher and the server
# binaries plus the built SPA. The server serves the SPA and the fetcher output
# from one origin; the fetcher runs as a CronJob writing to a shared volume the
# server reads.

# Stage 1: build the SPA. Default base path "/" suits server mode.
FROM node:20-alpine AS web
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: build the Go binaries.
FROM golang:1.25.5-bookworm AS build
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/fetcher ./cmd/fetcher \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -o /out/server ./cmd/server

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
