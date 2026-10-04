#!/usr/bin/make

SHELL = /bin/sh

test_coverage:
	rm -rf coverage-ci
	mkdir ./coverage-ci
	go test -p=1 -v -race -cover -tags=debug -timeout 30m -coverpkg=./... -coverprofile=./coverage-ci/summary.txt -covermode=atomic ./...

test: ## Run application tests
	go test -p=1 -v -race ./...
	go test -v -race -fuzz=FuzzStaticPoolEcho -fuzztime=30s -tags=debug ./pool/static_pool
