export GO111MODULE=on
export PATH:=/usr/local/go/bin:$(PATH)
exec_path := /usr/local/bin/
exec_name := docker-machine-driver-otc

VERSION := 0.3.0b1


default: test build
test: vet acceptance

fmt:
	@echo Running go fmt
	@go fmt

lint:
	@echo Running go lint
	@golint --set_exit_status ./...

vet:
	@echo "go vet ."
	@go vet $$(go list ./... | grep -v vendor/) ; if [ $$? -eq 1 ]; then \
		echo ""; \
		echo "Vet found suspicious constructs. Please check the reported constructs"; \
		echo "and fix them if necessary before submitting the code for review."; \
		exit 1; \
	fi

acceptance:
	@echo "Starting acceptance tests..."
	@go test ./... -race -covermode=atomic -coverprofile=coverage.txt -timeout 20m -v

build: build-linux

build-linux:
	@echo "Build driver for Linux"
	@go build --trimpath -o bin/$(exec_name)

build-windows:
	@echo "Build driver for Windows"
	@GOOS=windows go build --trimpath -o bin/$(exec_name).exe

build-all: build-linux build-windows

install:
	@cp ./bin/$(exec_name) $(exec_path)
	@echo "Driver installed to $(exec_path)$(exec_name)"

release: build-all
	@tar -czf "./bin/$(exec_name)-$(VERSION)-linux-amd64.tgz" "./bin/$(exec_name)"
	@zip -qmD ./bin/$(exec_name)-$(VERSION)-win-amd64.zip ./bin/$(exec_name).exe
	@echo "Release versions are built"
