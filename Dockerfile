FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/revive-dns ./cmd/revive-dns

FROM scratch
COPY --from=build /out/revive-dns /revive-dns
EXPOSE 53/udp 53/tcp
ENTRYPOINT ["/revive-dns", "-config", "/config.yaml"]
