# AMKR 本地路由服务的运行镜像。
#
# 配置、指标库、日志和 PID 文件都由 XDG_CACHE_HOME 推到 /data/auto-model-key-router，
# 挂一个卷就能整体持久化，也保证配置里的 API Key 不会被打进镜像（见 .dockerignore）。
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    XDG_CACHE_HOME=/data

WORKDIR /app
# setuptools 打包时要读 readme 与 license，所以这两个文件也得进构建上下文。
COPY pyproject.toml README.md LICENSE ./
COPY auto_model_key_router ./auto_model_key_router
RUN pip install --no-cache-dir . \
    && useradd --create-home --uid 10001 amkr \
    && mkdir -p /data \
    && chown amkr:amkr /data

USER amkr
VOLUME /data
EXPOSE 8000
# 必须覆盖 host：配置默认监听 127.0.0.1，那样 -p 8000:8000 转发不进容器。
# 端口仍由配置文件的 port 决定，改了端口就要同步改发布映射。
CMD ["amkr", "--host", "0.0.0.0", "--serve-foreground"]
