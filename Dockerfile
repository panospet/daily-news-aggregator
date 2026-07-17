# Build stage
FROM golang:alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/miniflux-dashboard .

# Run stage
FROM alpine:latest
RUN apk add --no-cache openssh-client ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/miniflux-dashboard .
COPY --from=builder /app/static/ ./static/
COPY --from=builder /app/templates/ ./templates/

EXPOSE 8080
CMD ["./miniflux-dashboard"]
