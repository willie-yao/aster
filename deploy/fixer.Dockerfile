# Fixer image: the engine fetcher plus the opencode CLI and git, for the
# agent-runtime fix-PR generator. Unlike the default distroless engine image,
# this uses a glibc base so opencode (a Bun-compiled binary) runs. Opt-in and
# separate; the default image stays minimal.
FROM golang:1.25.12-bookworm AS build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
ARG VERSION=fixer
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/fetcher ./cmd/fetcher

FROM node:20-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g opencode-ai \
    && opencode --version
COPY --from=build /out/fetcher /usr/local/bin/fetcher
# opencode writes config/data under HOME; the runtime uses isolated temp HOMEs,
# but give the non-root default a writable HOME too.
ENV HOME=/tmp
ENTRYPOINT ["/usr/local/bin/fetcher"]
