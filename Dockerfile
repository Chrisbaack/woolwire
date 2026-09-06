# Build web assets
FROM node:20-alpine AS web-builder
WORKDIR /app/web
COPY web/package*.json ./
RUN npm install
COPY web/ ./
RUN npm run build

# Build Go application
FROM golang:1.27-alpine AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /app/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/woolwire ./cmd/woolwire

# Final minimal production container
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 1000 woolwire && \
    adduser -u 1000 -G woolwire -s /bin/sh -D woolwire && \
    mkdir -p /state /models && \
    chown -R woolwire:woolwire /state /models

USER woolwire
WORKDIR /home/woolwire

COPY --from=go-builder /app/bin/woolwire /usr/local/bin/woolwire

EXPOSE 7070 4242
ENTRYPOINT ["/usr/local/bin/woolwire"]
CMD ["-listen", "0.0.0.0:7070", "-state", "/state"]
