# Build go plugin for Polarity EBS
FROM golang:1.24.5 AS builder

ARG GO_ARCH="amd64"
ARG DEBUG="false"

ENV DEBUG=${DEBUG}
ENV GO_ARCH=${GO_ARCH}

WORKDIR /app

COPY . .

RUN go clean && GOOS=linux GOARCH=${GO_ARCH} CGO_ENABLED=0 go build -ldflags="-s -w -X main.Debug=${DEBUG}" -o /bin/polarity-ecs-ebs-plugin ./cmd/plugin

# Create plugin rootfs
FROM alpine:latest AS rootfs

ARG DEBUG="false"
ENV DEBUG=${DEBUG}

RUN apk add --no-cache lsblk xfsprogs ca-certificates tzdata && \
    update-ca-certificates && \
    cp /usr/share/zoneinfo/UTC /etc/localtime && \
    echo "UTC" > /etc/timezone

COPY --from=builder /bin/polarity-ecs-ebs-plugin /bin/polarity-ecs-ebs-plugin

ENTRYPOINT ["/bin/sh"]
