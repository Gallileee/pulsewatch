# Build: a static binary (no cgo), so the image needs no libc.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /pulsewatch ./cmd/pulsewatch \
    && mkdir /data

# Run: distroless (CA certificates and tzdata, no shell), as a non-root user. /data belongs to
# that user, so a volume mounted there (which starts as a copy of it) is writable.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pulsewatch /pulsewatch
COPY --from=build --chown=65532:65532 /data /data
WORKDIR /data
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/pulsewatch"]
CMD ["-config", "/etc/pulsewatch/pulsewatch.yaml"]
