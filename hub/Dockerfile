# 中枢镜像（deploy/compose.hub.yml 在部署机上本地构建，本文件不单独发布镜像）。
# 纯 Go + modernc.org/sqlite：CGO_ENABLED=0 静态二进制，alpine 提供 wget（compose healthcheck）与 CA。
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/assistant-hub .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /out/assistant-hub /usr/local/bin/assistant-hub
EXPOSE 8795
# 配置全部经环境变量注入（见 deploy/hub.env.example）；数据卷挂 /data（ASSIST_DB_PATH=/data/assistant.db）。
ENTRYPOINT ["assistant-hub"]
