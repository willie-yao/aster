# Builds one of the experimental Orka binaries from the backend module. Select
# which with the CMD build-arg. Build context MUST be the backend/ directory.
#
# Long-lived shims (serve on :8080):
#   docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-artifact-tool \
#     -t orka-artifact-tool:latest backend/
#   docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-copilot-proxy \
#     -t orka-copilot-proxy:latest backend/
#
# Pipeline batch binaries (run to completion; take flags, see -h):
#   docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-producer \
#     -t orka-producer:latest backend/
#   docker build -f experimental/orka/Dockerfile --build-arg CMD=orka-ingestor \
#     -t orka-ingestor:latest backend/
FROM golang:1.25.12-bookworm AS build
ARG CMD=orka-artifact-tool
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
