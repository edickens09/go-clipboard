.PHONY: help
## help: shows this help message
help:
	@ echo "Usage: make [target]\n"
	@ sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' |  sed -e 's/^/ /'

.PHONY: test
## test: runs unit tests
test:
	@ go test -v ./... -count=1

.PHONY: build-sync
## build-sync: builds the clipsync command
build-sync:
	@ go build -o bin/ ./cmd/clipsync

.PHONY: install-sync
## install-sync: installs the clipsync command
install-sync:
	@ go install ./cmd/clipsync

.PHONY: copy-example
## copy-example: runs copy example
copy-example:
	@ go run examples/copy/copy.go

.PHONY: paste-example
## paste-example: runs paste example
paste-example:
	@ go run examples/paste/paste.go