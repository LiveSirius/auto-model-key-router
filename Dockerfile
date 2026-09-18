# AMKR 本地路由服务的运行镜像。
#
# 配置、指标库、日志和 PID 文件都由 XDG_CACHE_HOME 推到 /data/auto-model-key-router，
# 挂一个卷就能整体持久化，也保证配置里的 API Key 不会被打进镜像（见 .dockerignore）。
#
# 多阶段构建：前端资产经 //go:embed 编进二进制（见仓库根的 webui_assets.go），因此运行镜像
# 里既不需要 python 也不需要 node，更不需要把 webui/ 复制进去——静态资源已经在二进制内。
FROM golang:1.24 AS build

WORKDIR /src

# 先只复制依赖清单，让 go mod download 这一层能被缓存。
COPY go.mod go.sum ./
RUN go mod download

# 再复制源码。**webui/ 必须一起复制**：//go:embed webui 在编译期就要求该目录存在。
COPY . .

# CGO_ENABLED=0：SQLite 用的是纯 Go 的 modernc.org/sqlite，不需要 cgo，
# 关掉它才能得到静态链接的二进制、也才能在 slim 运行镜像里跑。
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/amkr ./cmd/amkr

FROM debian:bookworm-slim

# ca-certificates：版本检查要访问 PyPI/GitHub；
# tzdata：指标库的 created_at 按 Asia/Shanghai 计算，缺时区会退化成 UTC。
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 amkr \
    && mkdir -p /data \
    && chown amkr:amkr /data

ENV XDG_CACHE_HOME=/data

COPY --from=build /out/amkr /usr/local/bin/amkr

USER amkr
VOLUME /data
EXPOSE 8000
# 必须覆盖 host：配置默认监听 127.0.0.1，那样 -p 8000:8000 转发不进容器。
# 端口仍由配置文件的 port 决定，改了端口就要同步改发布映射。
CMD ["amkr", "--host", "0.0.0.0", "--serve-foreground"]
