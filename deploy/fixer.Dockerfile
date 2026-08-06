# Fixer image: the engine fetcher plus OpenCode, git, and the pinned local
# sandbox runtime for the agent-runtime fix-PR generator. Unlike the default
# distroless engine image, this uses a glibc base for OpenCode and srt. Opt-in
# and separate; the default image stays minimal.
FROM golang:1.25.12-bookworm AS build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
ARG VERSION=fixer
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/fetcher ./cmd/fetcher

FROM node:20-slim AS fixer-runtime
ARG OPENCODE_VERSION=1.18.2
ARG SRT_VERSION=0.0.70
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash bubblewrap ca-certificates git ripgrep socat \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "opencode-ai@${OPENCODE_VERSION}" "@anthropic-ai/sandbox-runtime@${SRT_VERSION}" \
    && opencode --version \
    && node -e "if (require('/usr/local/lib/node_modules/@anthropic-ai/sandbox-runtime/package.json').version !== '${SRT_VERSION}') process.exit(1)"
COPY --from=build /out/fetcher /usr/local/bin/fetcher
# opencode writes config/data under HOME; the runtime uses isolated temp HOMEs,
# but give the non-root default a writable HOME too.
ENV HOME=/tmp \
    SRT_BIN=/usr/local/bin/srt
ENTRYPOINT ["/usr/local/bin/fetcher"]
