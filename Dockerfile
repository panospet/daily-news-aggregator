# Build stage
FROM golang:1.23-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/miniflux-dashboard ./cmd/server

# Run stage
FROM python:3.11-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends openssh-client ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /opt/venv && \
    /opt/venv/bin/pip install --no-cache-dir numpy scikit-learn fastembed psycopg2-binary
WORKDIR /app
COPY --from=builder /app/miniflux-dashboard .
COPY --from=builder /app/static/ ./static/
COPY --from=builder /app/templates/ ./templates/
COPY cluster_articles.py .

EXPOSE 8080
ENV PATH="/opt/venv/bin:$PATH"
CMD ["./miniflux-dashboard"]
