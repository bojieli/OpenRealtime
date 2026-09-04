# syntax=docker/dockerfile:1

# The server image. It builds from source and ships nothing but the binary and
# the certificate roots, because a realtime server's attack surface should be
# the protocol it speaks and not a distribution's package set.
#
#   docker build -f deploy/Dockerfile -t openrealtime:1.0.0 .
#   docker run --rm -p 8080:8080 -e GEMINI_API_KEY -e OPENREALTIME_TOKEN \
#     openrealtime:1.0.0 \
#     serve -listen :8080 -binding upstream -upstream-provider google \
#     -slow-provider google

FROM golang:1.25 AS build

WORKDIR /src

# Dependencies resolve in their own layer so that a source-only change does not
# refetch the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Same flags as scripts/build-release.sh: static, trimmed, reproducible.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=" \
    -o /out/openrealtime ./cmd/openrealtime

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/openrealtime /usr/local/bin/openrealtime

# Non-root by default. The server needs no filesystem, no privileged port, and
# no capabilities; anything it does need is passed as configuration.
USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/openrealtime"]
CMD ["serve", "-listen", ":8080"]
