# 中枢镜像：compose.hub.yml 本地构建与 .github/workflows/release-hub.yml 的 GHCR 发布共用。
# 纯 Go + modernc.org/sqlite：CGO_ENABLED=0 静态二进制；BUILDPLATFORM + TARGETARCH 走 Go 交叉编译，
# 多架构构建无需 QEMU。alpine 提供 wget（compose healthcheck）与 CA。
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w" -o /out/assistant-hub .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /out/assistant-hub /usr/local/bin/assistant-hub
EXPOSE 8795
LABEL org.opencontainers.image.source="https://github.com/SakuraLoveSmile/Sakura-Assistant"
# 配置全部经环境变量注入（见 deploy/hub.env.example）；数据卷挂 /data（ASSIST_DB_PATH=/data/assistant.db）。
ENTRYPOINT ["assistant-hub"]
