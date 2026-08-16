# syntax=docker/dockerfile:1

# Build stage: compile a fully static apivo binary. CGO is disabled so the
# result runs on distroless/static, which ships no libc.
FROM golang:1.26 AS build
WORKDIR /src

# Modules first: source edits must not invalidate the download cache layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Version stamping (issue #119): the release workflow writes the annotated
# tag into a VERSION file at the context root (git-ignored, never committed)
# because `wrangler deploy` builds this image itself and offers no build-arg
# passthrough — a file in the context is the one channel every builder
# (wrangler, compose, CI, a bare docker build) shares. No file means "dev":
# the honest name for any image the release pipeline did not cut.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=$(cat VERSION 2>/dev/null || echo dev)" \
    -o /out/apivo ./cmd/apivo

# Final stage: distroless static, non-root. No shell, no package manager;
# the schema migrations are embedded in the binary itself.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=nonroot:nonroot /out/apivo /usr/local/bin/apivo

EXPOSE 8080

# Exec form is mandatory: there is no shell to interpret anything else. The
# binary probes its own /healthz, so the image needs no curl or wget.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/apivo", "healthcheck"]

USER nonroot
ENTRYPOINT ["/usr/local/bin/apivo"]
