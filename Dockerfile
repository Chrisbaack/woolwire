# Build web assets with the same Node major and lockfile-exact install as CI,
# so the bundle in the image matches the committed web/dist the release
# binaries embed.
#
# The build stages run on the build machine's own platform and cross-compile,
# so a multi-platform image build only emulates the short final stage. The UI
# bundle is platform-independent and Go needs no cgo here.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web-builder
WORKDIR /app/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Build Go application
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web/dist ./web/dist
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X github.com/Chrisbaack/woolwire/internal/buildinfo.version=${VERSION}" \
    -o /app/bin/woolwire ./cmd/woolwire

# Final minimal production container
FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 1000 woolwire && \
    adduser -u 1000 -G woolwire -s /bin/sh -D woolwire && \
    mkdir -p /state /models && \
    chown -R woolwire:woolwire /state /models

USER woolwire
WORKDIR /home/woolwire

COPY --from=go-builder /app/bin/woolwire /usr/local/bin/woolwire

EXPOSE 7070
ENTRYPOINT ["/usr/local/bin/woolwire"]
CMD ["-listen", "0.0.0.0:7070", "-state", "/state"]
