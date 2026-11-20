# syntax=docker/dockerfile:1

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w \
        -X github.com/cardeahq/cardea-gateway/internal/cli.version=${VERSION} \
        -X github.com/cardeahq/cardea-gateway/internal/cli.commit=${COMMIT} \
        -X github.com/cardeahq/cardea-gateway/internal/cli.date=${DATE}" \
      -o /out/gateway ./cmd/gateway

# Static distroless: CA certificates for upstream HTTPS, no shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=0:0 --chmod=0755 /out/gateway /usr/local/bin/gateway
# Config paths (configs/..., policies, identification dir) are relative to
# the working directory; mount the config tree here.
WORKDIR /app
# Run unprivileged: distroless's "nonroot" user (uid/gid 65532). Set
# explicitly so it survives a change of base image tag.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
CMD ["serve"]
