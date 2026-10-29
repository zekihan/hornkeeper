FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /hornkeeper ./cmd/hornkeeper

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /hornkeeper /hornkeeper
USER 25000:25000
EXPOSE 8080 8081
ENTRYPOINT ["/hornkeeper"]
LABEL org.opencontainers.image.source="https://github.com/zekihan/hornkeeper" \
      org.opencontainers.image.licenses="AGPL-3.0" \
      org.opencontainers.image.title="hornkeeper" \
      org.opencontainers.image.description="Opt-in PVC policy for Longhorn backup targets and replica counts"
