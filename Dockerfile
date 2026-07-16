# Pinned: newer xfsprogs (alpine:latest) defaults mkfs.xfs to XFS incompat features
# (parent pointers, exchange-range) that older LTS kernels (e.g. AL2023 6.1) refuse to
# mount. 3.20 ships xfsprogs 6.8, which keeps those off.
FROM alpine:3.20 AS rootfs

RUN apk add --no-cache lsblk xfsprogs ca-certificates tzdata && \
    update-ca-certificates && \
    cp /usr/share/zoneinfo/UTC /etc/localtime && \
    echo "UTC" > /etc/timezone

COPY ./dist/polarity-ecs-ebs-plugin /bin/polarity-ecs-ebs-plugin

ENTRYPOINT ["/bin/sh"]
