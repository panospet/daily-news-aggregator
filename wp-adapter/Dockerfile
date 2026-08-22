FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
# No go.sum since we don't have external dependencies
COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o wp-adapter main.go

FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/wp-adapter .

EXPOSE 8000
CMD ["./wp-adapter"]
