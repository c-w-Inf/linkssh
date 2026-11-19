BINARY  := linkssh
GO      ?= go
PORT    ?= 8080

.PHONY: all build run vet fmt tidy clean help

all: build

help:
	@printf '%s\n' \
	  'make build   - compile ./$(BINARY) (HTML is go:embed-ed, no codegen)' \
	  'make run     - build and serve (ARGS="user@host [-p 22] [-s secret] [-r dir]")' \
	  'make vet     - go vet ./...' \
	  'make fmt     - gofmt -w on local sources' \
	  'make tidy    - go mod tidy' \
	  'make clean   - remove the binary'

build:
	$(GO) build -trimpath -o $(BINARY) .

run: build
	./$(BINARY) -l $(PORT) $(ARGS)

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w $(filter %.go,$(wildcard *.go))

tidy:
	$(GO) mod tidy

clean:
	rm -f $(BINARY)
