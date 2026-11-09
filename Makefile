.PHONY: run build test integration fmt vet image

run:
	go run ./cmd/loyalty

build:
	go build -trimpath -o bin/loyalty ./cmd/loyalty

test:
	go test -race ./...

integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo "Set TEST_DATABASE_URL to a dedicated test database"; exit 1)
	go test -race -count=1 ./...

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...

image:
	podman build --format oci -f Containerfile -t washing-loyalty:local .
