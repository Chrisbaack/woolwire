FROM golang:1.27.1 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/m0probe ./cmd/m0probe

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install --no-install-recommends -y ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --home-dir /nonexistent --shell /usr/sbin/nologin woolwire \
    && mkdir -p /state /shared \
    && chown 65532:65532 /state /shared \
    && chmod 700 /state /shared
COPY --from=build /out/m0probe /usr/local/bin/m0probe
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/m0probe"]
