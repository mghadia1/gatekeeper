# Build a static binary, then copy it alone into a distroless image: no shell,
# no package manager, runs as a non-root user.

FROM golang:1.27-alpine AS build
WORKDIR /src
# Dependencies first, so code changes reuse the download layer.
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/gatekeeper ./cmd/gatekeeper

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gatekeeper /gatekeeper
USER nonroot:nonroot
EXPOSE 8080
# The image has no shell or curl; the binary checks itself.
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=3 \
    CMD ["/gatekeeper", "healthcheck"]
ENTRYPOINT ["/gatekeeper"]
