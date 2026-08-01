# Build stage
FROM golang:1.24-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/miniflux-dashboard ./cmd/server

# Run stage
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-client ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/miniflux-dashboard .
COPY --from=builder /app/static/ ./static/
COPY --from=builder /app/templates/ ./templates/

EXPOSE 8080
CMD ["./miniflux-dashboard"]
