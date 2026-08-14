# syntax=docker/dockerfile:1

# Build stage: compile a fully static apivo binary. CGO is disabled so the
# result runs on distroless/static, which ships no libc.
FROM golang:1.26 AS build
WORKDIR /src

# Modules first: source edits must not invalidate the download cache layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/apivo ./cmd/apivo

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
