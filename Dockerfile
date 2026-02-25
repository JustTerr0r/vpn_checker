# ---- Build stage ----
FROM golang:1.22-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /app/checker ./cmd/checker

# ---- Runtime stage ----
FROM alpine:3.19

RUN apk add --no-cache ca-certificates unzip wget && \
    wget -q -O /tmp/xray.zip \
      https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-64.zip && \
    unzip /tmp/xray.zip -d /usr/local/bin/ xray && \
    chmod +x /usr/local/bin/xray && \
    rm /tmp/xray.zip && \
    apk del unzip wget

WORKDIR /app

COPY --from=builder /app/checker /app/checker

ENTRYPOINT ["/app/checker"]
