FROM golang:1.25.12-alpine AS build
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/internal/runtime ./internal/runtime
COPY backend/internal/fixruntime/testdata/fakeexecutor ./internal/fixruntime/testdata/fakeexecutor
RUN CGO_ENABLED=0 go build -trimpath -o /out/fixruntime-fake ./internal/fixruntime/testdata/fakeexecutor

FROM alpine:3.22
RUN apk add --no-cache ca-certificates git \
 && addgroup -g 65532 nonroot \
 && adduser -D -H -u 65532 -G nonroot nonroot
COPY --from=build /out/fixruntime-fake /usr/local/bin/fixruntime-fake
ENV HOME=/tmp/home \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/fixruntime-fake"]
