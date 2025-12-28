#!/usr/bin/make

SHELL = /bin/sh

test_coverage:
	rm -rf coverage-ci
	mkdir ./coverage-ci
	go test -v -race -cover -tags=debug -timeout 30m -coverpkg=./... -coverprofile=./coverage-ci/pipe.out -covermode=atomic ./ipc/pipe
	go test -v -race -cover -tags=debug -timeout 30m -coverpkg=./... -coverprofile=./coverage-ci/socket.out -covermode=atomic ./ipc/socket
	go test -v -race -cover -tags=debug -timeout 30m -coverpkg=./... -coverprofile=./coverage-ci/pool_static.out -covermode=atomic ./pool/static_pool
	go test -v -race -cover -tags=debug -timeout 30m -coverpkg=./... -coverprofile=./coverage-ci/worker.out -covermode=atomic ./worker
	go test -v -race -cover -tags=debug -timeout 30m -coverpkg=./... -coverprofile=./coverage-ci/worker_stack.out -covermode=atomic ./worker_watcher
	go test -v -race -cover -tags=debug -timeout 30m -coverpkg=./... -coverprofile=./coverage-ci/channel.out -covermode=atomic ./worker_watcher/container/channel
	go test -v -race -cover -tags=debug -timeout 30m -coverpkg=./... -coverprofile=./coverage-ci/ratelimiter.out -covermode=atomic ./pool/ratelimiter
	echo 'mode: atomic' > ./coverage-ci/summary.txt
	tail -q -n +2 ./coverage-ci/*.out >> ./coverage-ci/summary.txt

test: ## Run application tests
	go test -v -race ./ipc/pipe
	go test -v -race ./ipc/socket
	go test -v -race ./pool/static_pool
	go test -v -race ./worker
	go test -v -race ./worker_watcher
	go test -v -race ./worker_watcher/container/channel
	go test -v -race ./pool/ratelimiter
	go test -v -race -fuzz=FuzzStaticPoolEcho -fuzztime=30s -tags=debug ./pool/static_pool
