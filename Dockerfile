FROM alpine:latest AS rootfs

RUN apk add --no-cache lsblk xfsprogs ca-certificates tzdata && \
    update-ca-certificates && \
    cp /usr/share/zoneinfo/UTC /etc/localtime && \
    echo "UTC" > /etc/timezone

COPY ./dist/polarity-ecs-ebs-plugin /bin/polarity-ecs-ebs-plugin

ENTRYPOINT ["/bin/sh"]
