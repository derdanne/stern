deps-download:
	go mod download

deps-clean:
	go mod tidy

build:
	export CGO_ENABLED=0 && \
    GOOS=linux GOARCH=amd64 go build \
	-installsuffix cgo \
	-ldflags="-s" \
    -o stern-develop

build-container:
	docker build . -t stern:develop
