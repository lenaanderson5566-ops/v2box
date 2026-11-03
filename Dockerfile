FROM golang:1.27.1-alpine AS builder
WORKDIR /app
COPY . .
ENV CGO_ENABLED=0 GOEXPERIMENT=jsonv2
RUN go build -tags with_quic,with_utls -trimpath -o /v2box .
FROM alpine:3.23
RUN apk --no-cache add ca-certificates tzdata && mkdir -p /etc/v2box
COPY --from=builder /v2box /usr/local/bin/v2box
COPY LICENSE LICENSE.sing-box NOTICE.md /usr/share/licenses/v2box/
COPY internal/licenses/ /usr/share/licenses/v2box/dependencies/
ENTRYPOINT ["v2box", "server", "--config", "/etc/v2box/config.json"]
