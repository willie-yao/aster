# Builds the dashboard-owned analyzer used by the isolated Orka container
# lifecycle experiment. Build context must be the backend directory.
FROM golang:1.25.12-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -o /out/analyzer ./cmd/analyzer

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/analyzer /analyzer
ENTRYPOINT ["/analyzer"]
