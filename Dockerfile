# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/paprika-adapter ./cmd/paprika-adapter \
 && mkdir -m 0700 /out/data

# Static binary, CA certificates, no shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/paprika-adapter /usr/local/bin/paprika-adapter
# A named volume mounted here is initialised with this ownership, so the
# unprivileged user can write the cache without any chown at run time.
COPY --from=build --chown=65532:65532 /out/data /data
ENV PAPRIKA_DATA_DIR=/data
VOLUME /data
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/paprika-adapter"]
CMD ["serve"]
