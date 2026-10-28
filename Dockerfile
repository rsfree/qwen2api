# 多阶段构建：静态二进制 + 最小运行镜像（无 shell、非 root）。
FROM golang:1.22-alpine AS build
WORKDIR /src

# 先拷 go.mod：没有第三方依赖，这一层永远命中缓存。
COPY go.mod ./
RUN go mod download

COPY . .
# TARGETOS/TARGETARCH 由 buildx 注入（多架构构建必需）。
# 🔴 不接这两个变量、写死 GOOS=linux 时，arm64 镜像里装的其实是 amd64 二进制 —
# 镜像能起、但跑起来是 "exec format error"，且只有拉到 arm 机器上才发现。
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
# -trimpath + -s -w：可复现 + 体积减半；版本号经 ldflags 注入。
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/qwen2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /app app
WORKDIR /app
COPY --from=build /out/qwen2api /usr/local/bin/qwen2api

USER app
EXPOSE 8400

# 容器内探活走 /healthz（不带凭据）。
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O- http://127.0.0.1:8400/healthz >/dev/null || exit 1

ENTRYPOINT ["qwen2api"]
