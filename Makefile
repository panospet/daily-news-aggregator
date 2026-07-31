.PHONY: build test clean

APP_NAME := miniflux-dashboard

## build: Compile the Go binary
build:
	go build -o $(APP_NAME) ./cmd/server

## test: Run all unit tests
test:
	go test ./... -v -count=1

## clean: Remove build artifacts
clean:
	rm -f $(APP_NAME)
