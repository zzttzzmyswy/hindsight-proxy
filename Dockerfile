# Build stage. CGO is off so the result is a static binary that runs on any
# linux/amd64 or linux/arm64 base without a libc dependency.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy the module files first so a code-only change reuses the dependency layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/hindsight-proxy ./cmd/hindsight-proxy

# Runtime stage. distroless static carries no shell and no package manager,
# which keeps the image small and the attack surface minimal; the NAS runs a
# memory service next to this proxy, so the proxy gets no privileges it does not
# need.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/hindsight-proxy /usr/local/bin/hindsight-proxy

# The routing table is mounted here. It holds credentials, so it is a mount and
# never part of the image.
VOLUME ["/etc/hindsight-proxy"]

EXPOSE 8890

# No HEALTHCHECK instruction: the compose file declares one, and Kubernetes
# probes the /healthz endpoint directly.
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/hindsight-proxy"]
