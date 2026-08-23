FROM python:3.12-alpine

WORKDIR /app
COPY kalodio.py .

# Run as unprivileged user
RUN adduser -D -u 10001 kalodio
USER kalodio

EXPOSE 8280
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8280/healthz || exit 1

CMD ["python3", "kalodio.py"]
