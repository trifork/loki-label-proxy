# Cross-compile from the build platform: with CGO disabled the arm64 image is a
# plain GOARCH retarget, so no QEMU emulation of the whole toolchain.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /loki-label-proxy ./cmd/loki-label-proxy

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /loki-label-proxy /loki-label-proxy
# AGPL section 13: the source of this program must be available to the users who
# interact with it over the network.
COPY LICENSE /LICENSE

EXPOSE 8080 8081
USER nonroot:nonroot

ENTRYPOINT ["/loki-label-proxy"]
