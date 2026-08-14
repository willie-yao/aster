# Dedicated one-shot dashboard analyzer image for Orka container Tasks.
FROM golang:1.25.12-bookworm AS build
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/analyzer ./cmd/analyzer

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/analyzer /usr/local/bin/analyzer
USER 65532:65532
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster Analyzer" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster"
ENTRYPOINT ["/usr/local/bin/analyzer"]
