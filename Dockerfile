# syntax=docker/dockerfile:1
#
# 多阶段构建:builder 用 Go 编译静态二进制,运行阶段用精简 alpine。
# Go 版本须 >= go.mod 声明的版本(当前 go 1.25.0)。

# ---- build stage ----
FROM golang:1.25-alpine AS builder

# 中国网络:用国内模块代理拉依赖,避免 docker build 时超时。海外部署可改回 "direct"。
ENV GOPROXY=https://goproxy.cn,direct \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

# 先只拷贝依赖清单并下载,利用层缓存(依赖不变则跳过重新下载)。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# 再拷贝源码并编译。-trimpath + -s -w 去调试信息,减小体积。
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/kiro-go .

# ---- runtime stage ----
FROM alpine:latest

# ca-certificates: 出站 HTTPS(Kiro/AWS/PG TLS)需要;tzdata: 正确的本地时间。
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /out/kiro-go /app/kiro-go
COPY --from=builder /src/web /app/web
COPY --from=builder /src/version.json /app/version.json

# 非 DB 配置(端口/密码/thinking 等)仍落 JSON,持久化到该目录(compose 挂载 volume)。
RUN mkdir -p /app/data

EXPOSE 8080
VOLUME ["/app/data"]

ENTRYPOINT ["/app/kiro-go"]
