# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mhk-server ./cmd/mhk-server \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mhk-server /usr/local/bin/mhk-server
# Owned by the nonroot user so a fresh named volume mounted here is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV MHK_LISTEN=:7350 MHK_DATA_DIR=/data
EXPOSE 7350/tcp 7350/udp
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mhk-server"]
CMD ["serve"]
