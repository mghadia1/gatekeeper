# Images from one build: `gatekeeper` (the default, last stage), `echo` (a
# test backend) and `loadgen` (the load generator for bench/). Each is a static binary alone in a distroless
# image: no shell, no package manager, a non-root user.

FROM golang:1.27-alpine AS build
WORKDIR /src
# Dependencies first, so code changes reuse the download layer.
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/gatekeeper ./cmd/gatekeeper \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/echo ./cmd/echo \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/loadgen ./cmd/loadgen

# The load generator, for bench/: run inside the Compose network so Docker
# Desktop's port forwarding from the host is not part of what is measured.
FROM gcr.io/distroless/static-debian12:nonroot AS loadgen
COPY --from=build /out/loadgen /loadgen
USER nonroot:nonroot
ENTRYPOINT ["/loadgen"]

FROM gcr.io/distroless/static-debian12:nonroot AS echo
COPY --from=build /out/echo /echo
USER nonroot:nonroot
EXPOSE 9000
HEALTHCHECK --interval=5s --timeout=3s --start-period=2s --retries=3 \
    CMD ["/echo", "healthcheck"]
ENTRYPOINT ["/echo"]

FROM gcr.io/distroless/static-debian12:nonroot AS gatekeeper
COPY --from=build /out/gatekeeper /gatekeeper
USER nonroot:nonroot
# 8080 public; 9091 admin (metrics), for the internal network only.
EXPOSE 8080 9091
# The image has no shell or curl; the binary checks itself.
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
    CMD ["/gatekeeper", "healthcheck"]
ENTRYPOINT ["/gatekeeper"]
