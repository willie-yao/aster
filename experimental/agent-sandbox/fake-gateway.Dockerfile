FROM golang:1.25.12-alpine AS build
WORKDIR /src
COPY backend/internal/fixexecutor/testdata/fakegateway/main.go ./main.go
RUN CGO_ENABLED=0 go build -trimpath -o /out/fake-model-gateway ./main.go

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fake-model-gateway /usr/local/bin/fake-model-gateway
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/fake-model-gateway"]
