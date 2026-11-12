# Builds the Sindri image from source: `docker build -t sindri .`
#
# Releases are built by goreleaser from deploy/docker/Dockerfile instead, which
# copies binaries goreleaser already compiled. Both end in the same distroless
# image, running as a non-root user with no shell.
FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/spj/sindri/internal/version.Version=${VERSION} -X github.com/spj/sindri/internal/version.Commit=${COMMIT} -X github.com/spj/sindri/internal/version.Date=${DATE}" \
    -o /out/sindri ./cmd/sindri

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/sindri /usr/local/bin/sindri
COPY --chown=65532:65532 deploy/docker/data/ /var/lib/sindri/
USER 65532:65532
EXPOSE 8080
VOLUME ["/var/lib/sindri"]
ENTRYPOINT ["/usr/local/bin/sindri"]
CMD ["serve", "--config", "/etc/sindri/sindri.yaml"]
