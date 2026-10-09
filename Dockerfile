# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/moripa-worlds ./cmd/moripa-worlds

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/moripa-worlds /moripa-worlds
USER 65532:65532
EXPOSE 25565 8080
ENTRYPOINT ["/moripa-worlds"]
