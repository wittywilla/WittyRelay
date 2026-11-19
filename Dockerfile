# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 WittyWilla and contributors
#
# A static binary on distroless: no shell, no package manager, runs as an
# unprivileged user. Built for each platform buildx asks for (amd64, arm64)
# by cross-compiling, so no emulation is needed.

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=0.1.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X github.com/wittywilla/wittyrelay/internal/relay.Version=${VERSION}" \
    -o /out/wittyrelay ./cmd/wittyrelay \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wittyrelay /usr/local/bin/wittyrelay
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV WITTYRELAY_DATA_DIR=/data WITTYRELAY_HTTP=:8080
VOLUME ["/data"]
EXPOSE 4001/udp 4001/tcp 8080/tcp
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/wittyrelay", "health"]
ENTRYPOINT ["/usr/local/bin/wittyrelay"]
