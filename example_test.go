// Package example contains runnable examples demonstrating dockerpool usage.
//
// These examples are designed to be both documentation and testable code.
// Run with: go test -v ./example/...
package dockerpool_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bermanf/dockerpool"
	"github.com/moby/moby/api/types/container"
)

// Example_basic demonstrates the simplest usage of dockerpool.
//
// This example shows:
//   - Creating a pool with minimal configuration
//   - Acquiring a container from the pool
//   - Executing a command and reading output
//   - Returning the container to the pool
//   - Graceful shutdown
func Example_basic() {
	ctx := context.Background()

	// Create pool with minimal required configuration
	pool, err := dockerpool.New(ctx, dockerpool.Config{
		Name:    "basic-pool",
		Network: dockerpool.NetworkConfig{Name: "basic-net", NeedsCreate: true},
		Image:   dockerpool.ImageConfig{Name: "alpine:latest", NeedsPull: true},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Shutdown(ctx)

	// Acquire a container from the pool
	// If pool is empty, a new container is created synchronously
	c, err := pool.Acquire(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// Execute a command inside the container
	result, err := c.Exec(ctx, []string{"echo", "Hello!"})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Print(string(result.Stdout))

	// Return the container to the pool for reuse
	// If pool is full (>= MaxIdle), container is removed instead
	pool.Return(ctx, c)
}

// Example_codeRunner demonstrates running user-provided code safely.
//
// This example shows:
//   - Pulling an image on pool creation
//   - Creating a network if it doesn't exist
//   - Using ExecStd with timeout and output limit
//   - Typical code runner / sandbox pattern
func Example_codeRunner() {
	ctx := context.Background()

	pool, err := dockerpool.New(ctx, dockerpool.Config{
		Name: "python-runner",
		Network: dockerpool.NetworkConfig{
			Name:        "runner-net",
			NeedsCreate: true, // Create network if not exists
		},
		Image: dockerpool.ImageConfig{
			Name:      "python:3.12-alpine",
			NeedsPull: true, // Pull image on pool creation
		},
		MinIdle: 10, // Keep 10 containers warm
		MaxIdle: 50, // Allow up to 50 idle containers
	})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Shutdown(ctx)

	// User-provided code (untrusted input)
	userCode :=
		`
		print("Hello from Python!")
			for i in range(3):
   			print(f"  iteration {i}")
		`

	c, err := pool.Acquire(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Return(ctx, c)

	// Execute with safety limits
	result, err := c.ExecStd(ctx,
		[]string{"python", "-c", userCode},
		dockerpool.ExecOptions{
			Timeout: 5 * time.Second, // Kill if runs longer than 5s
			Limit:   64 * 1024,       // 64KB max output
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Exit: %d\n%s", result.ExitCode, result.Stdout)
}

// Example_stdinPipe demonstrates passing data to a command via stdin.
//
// This example shows:
//   - Using ExecStream for streaming I/O
//   - Passing input via ExecOptions.Stdin
//   - Capturing output to a buffer
func Example_stdinPipe() {
	ctx := context.Background()

	pool, _ := dockerpool.New(ctx, dockerpool.Config{
		Name:    "pipe-pool",
		Network: dockerpool.NetworkConfig{Name: "pipe-net"},
		Image:   dockerpool.ImageConfig{Name: "alpine:latest"},
	})
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)
	defer pool.Return(ctx, c)

	// Data to process via stdin
	input := strings.NewReader("apple\nbanana\ncherry\napricot\n")

	var stdout bytes.Buffer
	exitCode, err := c.ExecStream(ctx,
		[]string{"grep", "^a"}, // Match lines starting with 'a'
		&stdout,
		nil, // Discard stderr
		dockerpool.ExecOptions{Stdin: input},
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("exit=%d\n%s", exitCode, stdout.String())
}

// Example_streamToFile demonstrates streaming command output directly to a file.
//
// This example shows:
//   - Using ExecStream with file writers
//   - Avoiding memory pressure for large outputs
//   - Real-time logging pattern
func Example_streamToFile() {
	ctx := context.Background()

	pool, _ := dockerpool.New(ctx, dockerpool.Config{
		Name:    "stream-pool",
		Network: dockerpool.NetworkConfig{Name: "stream-net"},
		Image:   dockerpool.ImageConfig{Name: "alpine:latest"},
	})
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)
	defer pool.Return(ctx, c)

	// Create log file for output
	logFile, _ := os.CreateTemp("", "build-*.log")
	defer os.Remove(logFile.Name())
	defer logFile.Close()

	// Stream output directly to file (no memory buffering)
	exitCode, _ := c.ExecStream(ctx,
		[]string{"sh", "-c", "echo 'Building...'; sleep 1; echo 'Done!'"},
		logFile,   // stdout → file
		os.Stderr, // stderr → console
		dockerpool.ExecOptions{Timeout: 30 * time.Second},
	)

	fmt.Printf("Build finished: exit=%d, log=%s\n", exitCode, logFile.Name())
}

// Example_resourceLimits demonstrates container resource constraints.
//
// This example shows:
//   - Setting memory and CPU limits via HostConfig
//   - Disabling network access for sandboxing
//   - Using container.Resources for fine-grained control
func Example_resourceLimits() {
	ctx := context.Background()

	pool, err := dockerpool.New(ctx, dockerpool.Config{
		Name:    "limited-pool",
		Network: dockerpool.NetworkConfig{Name: "limited-net"},
		Image:   dockerpool.ImageConfig{Name: "alpine:latest"},

		// Custom host configuration with resource limits
		HostConfig: &container.HostConfig{
			Resources: container.Resources{
				Memory:   128 * 1024 * 1024, // 128MB RAM limit
				NanoCPUs: 500_000_000,       // 0.5 CPU cores
			},
			// Disable networking inside container (sandbox mode)
			NetworkMode: "none",
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)
	defer pool.Return(ctx, c)

	// Network access will fail due to NetworkMode: "none"
	result, _ := c.Exec(ctx, []string{"ping", "-c1", "8.8.8.8"})
	fmt.Printf("Network disabled: exit=%d\n", result.ExitCode)
}

// Example_customLabels demonstrates adding custom labels to containers.
//
// This example shows:
//   - Adding custom labels via Config.Labels
//   - Reading labels from acquired containers
//   - Using labels for monitoring/filtering
func Example_customLabels() {
	ctx := context.Background()

	pool, _ := dockerpool.New(ctx, dockerpool.Config{
		Name:    "labeled-pool",
		Network: dockerpool.NetworkConfig{Name: "labeled-net"},
		Image:   dockerpool.ImageConfig{Name: "alpine:latest"},

		// Custom labels added to all pool containers
		Labels: map[string]string{
			"app":         "code-runner",
			"environment": "production",
			"team":        "platform",
		},
	})
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)
	defer pool.Return(ctx, c)

	// Labels are accessible on the container
	for k, v := range c.Labels() {
		fmt.Printf("%s=%s\n", k, v)
	}
}

// Example_errorHandling demonstrates proper error handling patterns.
//
// This example shows:
//   - Handling Docker API errors vs command exit codes
//   - When to use Return() vs Remove()
//   - Using OnError callback for async errors
func Example_errorHandling() {
	ctx := context.Background()

	pool, _ := dockerpool.New(ctx, dockerpool.Config{
		Name:    "error-pool",
		Network: dockerpool.NetworkConfig{Name: "error-net"},
		Image:   dockerpool.ImageConfig{Name: "alpine:latest"},

		// Callback for async errors (refill failures, removal errors, etc.)
		OnError: func(err error) {
			log.Printf("[POOL ERROR] %v", err)
		},
	})
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)

	// Execute a command that exits with error
	result, err := c.Exec(ctx, []string{"sh", "-c", "exit 1"})

	if err != nil {
		// Docker API error (container issue, network error, etc.)
		// Container might be in bad state — remove, don't reuse
		pool.Remove(ctx, c)
		return
	}

	if result.ExitCode != 0 {
		// Command failed, but container is healthy
		// Safe to return to pool
		fmt.Printf("Command failed: exit=%d, stderr=%s\n", result.ExitCode, result.Stderr)
		pool.Return(ctx, c)
		return
	}

	// Success
	pool.Return(ctx, c)
}

// Example_gracefulShutdown demonstrates proper shutdown with signal handling.
//
// This example shows:
//   - Handling SIGINT/SIGTERM for graceful shutdown
//   - Using context timeout for shutdown deadline
//   - Pattern for long-running services
func Example_gracefulShutdown() {
	ctx := context.Background()

	pool, _ := dockerpool.New(ctx, dockerpool.Config{
		Name:    "shutdown-pool",
		Network: dockerpool.NetworkConfig{Name: "shutdown-net"},
		Image:   dockerpool.ImageConfig{Name: "alpine:latest"},
	})

	// Set up signal handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Run worker in background
	done := make(chan struct{})
	go func() {
		defer close(done)

		for i := 0; i < 100; i++ {
			c, err := pool.Acquire(ctx)
			if err != nil {
				return // Pool is shutting down
			}

			// Do work...
			c.Exec(ctx, []string{"sleep", "0.1"})

			pool.Return(ctx, c)
		}
	}()

	// Wait for signal or completion
	select {
	case <-sigCh:
		fmt.Println("Received signal, shutting down...")
	case <-done:
		fmt.Println("Work complete")
	}

	// Graceful shutdown with timeout
	// Waits for all in-use containers to be returned
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := pool.Shutdown(shutdownCtx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
}

// Example_metrics demonstrates monitoring pool state.
//
// This example shows:
//   - Reading IdleCount() and InUse() metrics
//   - Pattern for exposing metrics to monitoring systems
//   - Observing pool behavior under load
func Example_metrics() {
	ctx := context.Background()

	pool, _ := dockerpool.New(ctx, dockerpool.Config{
		Name:    "metrics-pool",
		Network: dockerpool.NetworkConfig{Name: "metrics-net"},
		Image:   dockerpool.ImageConfig{Name: "alpine:latest"},
		MinIdle: 5,
		MaxIdle: 20,
	})
	defer pool.Shutdown(ctx)

	// Metrics reporter (could export to Prometheus, etc.)
	go func() {
		for {
			time.Sleep(time.Second)
			fmt.Printf("pool_idle=%d pool_in_use=%d\n",
				pool.IdleCount(),
				pool.InUse(),
			)
		}
	}()

	// Simulate burst of requests
	for i := 0; i < 10; i++ {
		c, _ := pool.Acquire(ctx)
		go func() {
			time.Sleep(500 * time.Millisecond)
			pool.Return(ctx, c)
		}()
	}

	time.Sleep(2 * time.Second)
}

// Example_highThroughput demonstrates configuration for high-load scenarios.
//
// This example shows:
//   - Tuning MinIdle/MaxIdle for burst traffic
//   - Adjusting RefillInterval for faster recovery
//   - Increasing MaxConcurrentDockerOps for parallel operations
//   - Production-ready error handling
func Example_highThroughput() {
	ctx := context.Background()

	pool, err := dockerpool.New(ctx, dockerpool.Config{
		Name: "high-throughput",
		Network: dockerpool.NetworkConfig{
			Name:        "ht-net",
			NeedsCreate: true,
		},
		Image: dockerpool.ImageConfig{Name: "alpine:latest"},

		// Large pool for handling burst traffic
		MinIdle: 100, // Always keep 100 containers warm
		MaxIdle: 500, // Allow up to 500 idle containers

		// Aggressive refill for fast recovery
		RefillInterval:         50 * time.Millisecond,
		MaxConcurrentDockerOps: 50, // Parallel Docker operations

		// Production error handling
		OnError: func(err error) {
			// Send to monitoring system (Prometheus, Datadog, etc.)
			log.Printf("Pool error: %v", err)
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Shutdown(ctx)

	// Ready for high throughput...
}

// Example_networkIsolation demonstrates creating isolated network per pool.
//
// This example shows:
//   - Creating a dedicated network for pool containers
//   - Automatic network cleanup on shutdown
//   - Containers can communicate within the network
func Example_networkIsolation() {
	ctx := context.Background()

	pool, err := dockerpool.New(ctx, dockerpool.Config{
		Name: "isolated-pool",
		Network: dockerpool.NetworkConfig{
			Name:        "isolated-net-" + fmt.Sprint(time.Now().Unix()),
			Driver:      "bridge",
			NeedsCreate: true, // Create network on pool start
			NeedsRemove: true, // Remove network on shutdown
			Labels: map[string]string{
				"purpose": "isolation",
			},
		},
		Image: dockerpool.ImageConfig{Name: "alpine:latest"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Shutdown(ctx) // Also removes the network

	c, _ := pool.Acquire(ctx)
	defer pool.Return(ctx, c)

	// Containers in this pool can communicate via the isolated network
	// but are isolated from other Docker networks
}
