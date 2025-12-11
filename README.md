# Dockerpool

A Go library for managing a pool of reusable Docker containers with automatic refill and graceful shutdown.

## Features

- **Container pooling** — pre-created containers ready for immediate use
- **Auto-refill** — background worker maintains minimum idle containers
- **Graceful shutdown** — waits for in-use containers before cleanup
- **Concurrent operations limit** — prevents Docker daemon overload
- **Container sync on restart** — recovers existing pool containers by labels
- **Race-free design** — safe for concurrent use from multiple goroutines

## Installation

```bash
go get github.com/bermanf/dockerpool
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/bermanf/dockerpool"
)

func main() {
    ctx := context.Background()

    // Create pool with minimal config
    pool, err := dockerpool.New(ctx, dockerpool.Config{
        Name:    "my-pool",
        Network: dockerpool.NetworkConfig{Name: "my-network", NeedsCreate: true},
        Image:   dockerpool.ImageConfig{Name: "alpine:latest", NeedsPull: true},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer pool.Shutdown(ctx)

    // Acquire container from pool
    container, err := pool.Acquire(ctx)
    if err != nil {
        log.Fatal(err)
    }

    // Execute command
    result, err := container.Exec(ctx, []string{"echo", "Hello!"})
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(string(result.Stdout)) // Hello!

    // Return container to pool for reuse
    pool.Return(ctx, container)
}
```

## Configuration

### Full Config Example

```go
pool, err := dockerpool.New(ctx, dockerpool.Config{
    // Required
    Name: "my-pool",  // Pool name (used for container labels)
    
    Network: dockerpool.NetworkConfig{
        Name:        "my-network",    // Docker network name
        Driver:      "bridge",        // Network driver (default: bridge)
        NeedsCreate: true,            // Create network if not exists
        NeedsRemove: true,            // Remove network on shutdown
        Labels:      map[string]string{"env": "dev"},
    },
    
    Image: dockerpool.ImageConfig{
        Name:      "python:3.12-alpine", // Container image
        NeedsPull: true,                  // Pull image on pool creation
    },

    // Optional — have sensible defaults
    Cmd:                    []string{"sh"},             // Container command (default: ["sleep", "infinity"])
    MinIdle:                10,                         // Min idle containers (default: 5)
    MaxIdle:                100,                        // Max idle containers (default: 10)
    RefillInterval:         50 * time.Millisecond,      // Refill check interval (default: 100ms)
    MaxConcurrentDockerOps: 20,                         // Parallel Docker ops in refill/shutdown (default: 10)
    MaxConcurrentAcquire:   20,                         // Parallel container creation in Acquire (default: 10)

    // Advanced — custom container configuration
    ContainerConfig: &container.Config{
        Image: "python:3.12-alpine",
        Env:   []string{"PYTHONUNBUFFERED=1"},
    },
    HostConfig: &container.HostConfig{
        Resources: container.Resources{
            Memory:   128 * 1024 * 1024, // 128MB RAM limit
            NanoCPUs: 500_000_000,       // 0.5 CPU
        },
        NetworkMode: "none", // Disable networking inside container
    },

    // Custom labels for containers
    Labels: map[string]string{
        "app":         "code-runner",
        "environment": "production",
    },

    // Error callback (default: log.Printf)
    OnError: func(err error) {
        log.Printf("[POOL ERROR] %v", err)
    },
})
```

### Config Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `Name` | `string` | **required** | Pool name, used for container labels |
| `Network` | `NetworkConfig` | **required** | Docker network configuration |
| `Image` | `ImageConfig` | **required** | Container image configuration |
| `Cmd` | `[]string` | `["sleep", "infinity"]` | Command to run in containers |
| `MinIdle` | `int` | `5` | Minimum idle containers to maintain |
| `MaxIdle` | `int` | `10` | Maximum idle containers (excess removed) |
| `RefillInterval` | `time.Duration` | `100ms` | How often to check and refill pool |
| `MaxConcurrentDockerOps` | `int` | `10` | Max parallel Docker API calls (refill, shutdown) |
| `MaxConcurrentAcquire` | `int` | `10` | Max parallel container creation in Acquire when pool is empty |
| `ContainerConfig` | `*container.Config` | `nil` | Custom container config (overrides Image, Cmd) |
| `HostConfig` | `*container.HostConfig` | `nil` | Custom host config (resources, volumes) |
| `Labels` | `map[string]string` | `nil` | Additional container labels |
| `OnError` | `func(error)` | `log.Printf` | Error callback for async errors |

## Command Execution

### Simple Execution

```go
result, err := container.Exec(ctx, []string{"ls", "-la"})
if err != nil {
    // Docker API error
}
fmt.Printf("Exit: %d\nStdout: %s\nStderr: %s\n", 
    result.ExitCode, result.Stdout, result.Stderr)
```

### With Timeout and Output Limit

```go
result, err := container.ExecStd(ctx, []string{"python", "-c", userCode}, 
    dockerpool.ExecOptions{
        Timeout: 5 * time.Second,  // Kill after 5s
        Limit:   64 * 1024,        // 64KB max output
    },
)
if errors.Is(err, dockerpool.ErrOutputLimitExceeded) {
    // Output was truncated
}
```

### Streaming Output

For large outputs or real-time logging, use `ExecStream`:

```go
exitCode, err := container.ExecStream(ctx,
    []string{"sh", "-c", "echo building... && make && echo done"},
    os.Stdout,  // stdout writer
    os.Stderr,  // stderr writer  
    dockerpool.ExecOptions{Timeout: 5 * time.Minute},
)
```

### With Stdin

```go
input := strings.NewReader("line1\nline2\nline3\n")

var stdout bytes.Buffer
exitCode, err := container.ExecStream(ctx, 
    []string{"grep", "line2"},
    &stdout, 
    io.Discard,
    dockerpool.ExecOptions{Stdin: input},
)
fmt.Println(stdout.String()) // line2
```

## Pool Operations

### Acquire and Return

```go
// Acquire container (from pool or creates new if empty)
container, err := pool.Acquire(ctx)
if errors.Is(err, dockerpool.ErrPoolShutdown) {
    // Pool is shutting down
}

// Return to pool (or removes if pool is full)
pool.Return(ctx, container)

// Remove without returning to pool (e.g., after error)
pool.Remove(ctx, container)
```

### Metrics

```go
idle := pool.IdleCount()  // Containers waiting in pool
inUse := pool.InUse()     // Containers currently acquired
```

### Graceful Shutdown

```go
// Shutdown waits for:
// 1. All in-use containers to be returned
// 2. All async removal operations to complete
// 3. Removes all idle containers
// 4. Optionally removes network

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := pool.Shutdown(ctx); err != nil {
    log.Printf("Shutdown error: %v", err)
}
```

## Error Handling

```go
container, err := pool.Acquire(ctx)
if err != nil {
    switch {
    case errors.Is(err, dockerpool.ErrPoolShutdown):
        // Pool is shut down, cannot acquire
    case errors.Is(err, context.DeadlineExceeded):
        // Context timeout
    default:
        // Docker API error
    }
}
```

## Errors

| Error | Description |
|-------|-------------|
| `ErrPoolShutdown` | Returned when calling Acquire on a shut down pool |
| `ErrPoolNameRequired` | Config validation: Name is empty |
| `ErrNetworkNameRequired` | Config validation: Network.Name is empty |
| `ErrImageRequired` | Config validation: Image.Name is empty |
| `ErrOutputLimitExceeded` | Exec output exceeded the specified Limit |

## Performance Tips

1. **Tune MinIdle/MaxIdle** — Set `MinIdle` high enough to handle burst traffic without synchronous container creation

2. **Adjust RefillInterval** — Lower values mean faster recovery but more CPU usage; `50-100ms` is a good range

3. **Limit concurrent ops** — `MaxConcurrentDockerOps` prevents overwhelming Docker daemon; increase for powerful servers

4. **Use ExecStream for large outputs** — Avoids buffering entire output in memory

5. **Reuse containers** — Always `Return()` healthy containers instead of `Remove()` to benefit from pooling

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        DockerPool                           │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐  │
│  │   Config    │  │   Watcher   │  │   Pool (Stack)      │  │
│  │  (settings) │  │  (refill)   │  │   LIFO for cache    │  │
│  └─────────────┘  └─────────────┘  └─────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                            │
                  Acquire() │ Return()
                            ▼
┌─────────────────────────────────────────────────────────────┐
│                       Container                              │
│         Exec() / ExecStd() / ExecStream()                   │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────┐
│                     Docker Daemon                            │
└─────────────────────────────────────────────────────────────┘
```

## Thread Safety

All pool operations are safe for concurrent use:
- `Acquire()`, `Return()`, `Remove()` — can be called from any goroutine
- `Shutdown()` — blocks new `Acquire()` calls, waits for in-flight operations
- `IdleCount()`, `InUse()` — atomic reads

## License

MIT
