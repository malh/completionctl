.PHONY: build install test
build:
	go build -o bin/completionctl .
install:
	go install .
test:
	go test ./...
	go build -o /tmp/completionctl-test .
	set -e; for t in tests/*.zsh; do zsh "$$t" /tmp/completionctl-test; done
	rm -f /tmp/completionctl-test
