FROM golang:1.27-bookworm AS builder
WORKDIR /src
COPY go.mod ./
# 第一次构建需要能访问 proxy.golang.org / Go 模块代理
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -mod=mod -trimpath -ldflags="-s -w" -o /usr/local/bin/monitor .

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S monitor && adduser -S -G monitor monitor && \
    mkdir -p /data && chown monitor:monitor /data
COPY --from=builder /usr/local/bin/monitor /usr/local/bin/monitor
USER monitor
ENV DATA_DIR=/data LISTEN_ADDR=:8080 TZ=Asia/Shanghai
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/monitor"]
