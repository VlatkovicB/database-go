.PHONY: run build test clean

run:
	@lsof -ti :8080 | xargs kill -9 2>/dev/null || true
	go run ./cmd/server/

build:
	go build -o minidb ./cmd/server/

test:
	go test ./...

clean:
	rm -f minidb
