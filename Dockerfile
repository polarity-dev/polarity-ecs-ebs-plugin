ARG GO_ARCH="amd64"

# --- Fase 1: build binario Go ---
FROM golang:1.24.5 AS builder

WORKDIR /app

# Copia i sorgenti
COPY . .

# Compila il binario statico
RUN go clean && GOOS=linux GOARCH=${GO_ARCH} CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/polarity-ebs-plugin ./cmd/plugin


# --- Fase 2: rootfs del plugin ---
FROM alpine:latest

RUN apk add nvme-cli lsblk xfsprogs
# Installa certificati e sincronizza l'orologio
RUN apk add --no-cache ca-certificates tzdata && \
    update-ca-certificates && \
    cp /usr/share/zoneinfo/UTC /etc/localtime && \
    echo "UTC" > /etc/timezone

# Copia il binario del plugin
COPY --from=builder /bin/polarity-ebs-plugin /bin/polarity-ebs-plugin

# Imposta entrypoint del plugin
ENTRYPOINT ["/bin/polarity-ebs-plugin"]
