.PHONY: build test bench proto clean run

build:
	go build -o bin/server ./cmd/server
	go build -o bin/client ./cmd/client
	go build -o bin/bench ./cmd/bench

run: build
	./bin/server

test:
	go test ./... -v

bench: build
	./bin/server &
	sleep 1
	./bin/bench
	@kill %1 2>/dev/null || true

proto:
	protoc --go_out=. --go_opt=paths=source_relative proto/schema.proto

clean:
	rm -rf bin/
