ARG GO_VERSION=1.26.1

FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/http-probe \
    .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

RUN addgroup -S http-probe && adduser -S -G http-probe http-probe
USER http-probe

EXPOSE 9100

COPY --from=builder /out/http-probe /usr/local/bin/http-probe

ENTRYPOINT ["/usr/local/bin/http-probe"]
CMD ["-config", "/etc/http-probe/config.json"]
