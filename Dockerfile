FROM golang:1.26.6-bookworm AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/manager ./cmd/manager
RUN GOBIN=/out go install github.com/kopia/kopia@v0.23.1
FROM debian:bookworm-slim
LABEL org.opencontainers.image.licenses="Apache-2.0"
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata bash jq openssl util-linux && rm -rf /var/lib/apt/lists/* && useradd -u 10001 -m manager
RUN mkdir -p /data /bootstrap /extensions && chown 10001:10001 /data /bootstrap /extensions
COPY --from=build /out/manager /out/kopia /usr/local/bin/
COPY LICENSE NOTICE /usr/local/share/licenses/manager/
USER 10001:10001
WORKDIR /home/manager
ENV KOPIA_UI_PROXY_LISTEN=:8081 KOPIA_UI_PROXY_TARGET=https://kopia:51515 KOPIA_UI_PROXY_PIN_FILE=/bootstrap/connection.json
EXPOSE 8080 8081
COPY docker/manager-entrypoint.sh /usr/local/bin/manager-entrypoint
ENTRYPOINT ["/usr/local/bin/manager-entrypoint"]
CMD ["serve-auto"]
