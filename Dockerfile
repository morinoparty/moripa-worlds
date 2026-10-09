# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
# -tags musl: Gate の ViaLite (go.minekube.com/vialite) は purego 経由で libdl を動的リンクさせ、
# CGO_ENABLED=0 でも distroless/static では起動できない ("exec /moripa-worlds: no such file or directory")。
# Gate 自身の musl ビルドと同じタグで ViaLite と OpenTelemetry を外し、静的バイナリにする
# (どちらも使わない。Gate の設定で via を有効にすると起動時に失敗する)
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -tags musl -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/moripa-worlds ./cmd/moripa-worlds
# 動的リンクに戻っていたら (依存の更新などで) ここで止める
RUN ldd /out/moripa-worlds 2>&1 | grep -q "not a dynamic executable"

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/moripa-worlds /moripa-worlds
USER 65532:65532
EXPOSE 25565 8080
# controller / proxy をサブコマンドで選ぶ (Kubernetes の args: [controller] / [proxy])
ENTRYPOINT ["/moripa-worlds"]
