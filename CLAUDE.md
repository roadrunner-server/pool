# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the RoadRunner Pool library - a Go package for managing PHP worker processes in the RoadRunner application server. It provides process management, IPC communication, and worker lifecycle control.

## Key Commands

### Testing
```bash
# Run all tests with race detection
make test

# Run tests with coverage
make test_coverage

# Run specific package tests
go test -v -race ./pool/static_pool
go test -v -race ./worker
go test -v -race ./ipc/pipe
go test -v -race ./ipc/socket
go test -v -race ./worker_watcher

# Run fuzz tests (30 seconds)
go test -v -race -fuzz=FuzzStaticPoolEcho -fuzztime=30s -tags=debug ./pool/static_pool
```

### Linting
```bash
# Run golangci-lint (with race build tags)
golangci-lint run --timeout=10m --build-tags=race
```

### Dependencies
```bash
# Update dependencies
go mod tidy
go mod download
```

## Architecture Overview

### Core Components

1. **Worker Process (`worker/`)**: Manages individual PHP worker processes
   - `Process` struct handles process lifecycle, communication via goridge relay
   - Uses FSM (Finite State Machine) for state management
   - Implements sync.Pool for efficient memory reuse

2. **Static Pool (`pool/static_pool/`)**: Fixed-size worker pool implementation
   - Manages a pool of workers with configurable size
   - Handles task distribution and worker lifecycle
   - Supports supervised mode with TTL and memory limits
   - Dynamic allocation support for scaling workers based on load

3. **IPC Layer (`ipc/`)**: Inter-process communication implementations
   - `pipe/`: Named pipe communication (Unix sockets on Linux/Mac, named pipes on Windows)
   - `socket/`: TCP socket communication
   - Both implement the `Factory` interface for worker creation

4. **Worker Watcher (`worker_watcher/`)**: Monitors and manages worker states
   - Tracks worker TTLs, idle time, and memory usage
   - Handles worker allocation and deallocation
   - Implements container-based worker storage

5. **FSM (`fsm/`)**: Finite State Machine for worker state tracking
   - States: Inactive, Ready, Working, Stopped, Errored, Invalid
   - Thread-safe state transitions with atomic operations

6. **Payload (`payload/`)**: Message payload handling between workers and pool
   - Manages request/response serialization
   - Handles context propagation

### Key Interfaces

- `pool.Pool`: Main pool interface for worker management
- `pool.Factory`: Interface for creating worker connections
- `pool.Command`: Interface for creating worker commands
- `relay.Relay`: Communication interface (goridge)

### Configuration

Pool configuration supports:
- Number of workers (`NumWorkers`)
- Max jobs per worker (`MaxJobs`)
- Queue size limits (`MaxQueueSize`)
- Various timeouts (allocate, destroy, reset, stream)
- Supervisor settings (TTL, memory limits, watch intervals)
- Dynamic allocation options (max workers, spawn rate, idle timeout)

### Testing Approach

- Uses PHP test workers in `tests/` directory
- Extensive unit tests with race detection
- Fuzz testing for reliability
- Coverage reporting via codecov
- Tests run on multiple OS platforms (Linux, macOS, Windows)