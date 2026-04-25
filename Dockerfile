FROM golang:1.24 AS controller-build

WORKDIR /src

COPY controller/go.mod controller/go.sum ./
RUN go mod download

COPY controller/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/mdns-controller .

FROM debian:13-slim

ENV DEBIAN_FRONTEND="noninteractive"

RUN apt-get update \
    && apt-get install --no-install-recommends -y \
        avahi-daemon \
        avahi-utils \
        dbus \
        iproute2 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --chmod=755 entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --from=controller-build /out/mdns-controller /usr/local/bin/mdns-controller

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
