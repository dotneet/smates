FROM python:3.13-slim
COPY --from=ghcr.io/astral-sh/uv:0.13 /uv /bin/uv
WORKDIR /app
COPY . .
RUN uv sync --frozen --no-dev
CMD ["uv", "run", "shop", "summary"]
