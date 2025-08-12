ARG GO_ARCH="amd64"

# Build go plugin for Polarity EBS
FROM golang:1.24.5 AS builder

WORKDIR /app

COPY . .

RUN go clean && GOOS=linux GOARCH=${GO_ARCH} CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/polarity-ecs-ebs-plugin ./cmd/plugin

# Create plugin rootfs
FROM alpine:latest

RUN apk add --no-cache nvme-cli lsblk xfsprogs
RUN apk add --no-cache ca-certificates tzdata && \
    update-ca-certificates && \
    cp /usr/share/zoneinfo/UTC /etc/localtime && \
    echo "UTC" > /etc/timezone

COPY --from=builder /bin/polarity-ecs-ebs-plugin /bin/polarity-ecs-ebs-plugin

ENTRYPOINT ["/bin/polarity-ecs-ebs-plugin"]
