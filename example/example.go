package example

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bermanf/dockerpool"
	"github.com/moby/moby/api/types/container"
)

// Example_basic demonstrates basic pool usage
func Example_basic() {
	ctx := context.Background()

	// Create Docker client
	docker, err := dockerpool.NewDockerClient()
	if err != nil {
		panic(err)
	}
	defer docker.Close()

	// Ensure network exists
	if _, err := docker.EnsureNetwork(ctx, "my-network"); err != nil {
		panic(err)
	}

	// Pull image (required before creating containers)
	if err := docker.PullImage(ctx, "alpine:latest"); err != nil {
		panic(err)
	}

	// Create pool with default config
	config := dockerpool.DefaultDockerPoolConfig()
	config.MinIdle = 5
	config.MaxIdle = 10

	pool, err := dockerpool.NewDockerPool(ctx, docker, "my-pool", "my-network", config)
	if err != nil {
		panic(err)
	}
	defer pool.Shutdown(ctx)

	// Acquire container from pool
	c, err := pool.Acquire(ctx)
	if err != nil {
		panic(err)
	}

	// Execute command
	result, err := c.Exec(ctx, []string{"echo", "Hello from container!"})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Stdout)

	// Return container to pool
	pool.Return(ctx, c)
}

// Example_customConfig demonstrates pool with custom configuration
func Example_customConfig() {
	ctx := context.Background()

	docker, _ := dockerpool.NewDockerClient()
	defer docker.Close()

	// Ensure network exists
	docker.EnsureNetwork(ctx, "app-network")

	// Pull custom image
	docker.PullImage(ctx, "python:3.11-alpine")

	// Custom configuration
	config := dockerpool.DockerPoolConfig{
		MinIdle:                100,
		MaxIdle:                1000,
		RefillInterval:         50 * time.Millisecond,
		MaxConcurrentDockerOps: 20,
		ContainerConfig: dockerpool.CreateContainerOptions{
			Config: &container.Config{
				Image: "python:3.11-alpine",
				Cmd:   []string{"sleep", "infinity"},
			},
		},
		Labels: map[string]string{
			"env":     "production",
			"service": "code-runner",
		},
		OnError: func(err error) {
			fmt.Fprintf(os.Stderr, "Pool error: %v\n", err)
		},
	}

	pool, _ := dockerpool.NewDockerPool(ctx, docker, "python-pool", "app-network", config)
	defer pool.Shutdown(ctx)

	// Use pool...
}

// Example_execStd demonstrates executing command and getting string output
func Example_execStd() {
	ctx := context.Background()

	docker, _ := dockerpool.NewDockerClient()
	defer docker.Close()

	docker.EnsureNetwork(ctx, "network")
	docker.PullImage(ctx, "alpine:latest")

	pool, _ := dockerpool.NewDockerPool(ctx, docker, "pool", "network", dockerpool.DefaultDockerPoolConfig())
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)
	defer pool.Return(ctx, c)

	// Execute with options - returns strings
	result, err := c.ExecStd(ctx, []string{"cat", "/etc/os-release"}, dockerpool.ExecOptions{
		Timeout: 5 * time.Second,
		Limit:   1024 * 10, // 10KB max output
	})
	if err != nil {
		panic(err)
	}

	fmt.Printf("Exit code: %d\n", result.ExitCode)
	fmt.Printf("Stdout: %s\n", result.Stdout)
	fmt.Printf("Stderr: %s\n", result.Stderr)
}

// Example_execStream demonstrates streaming output to writers
func Example_execStream() {
	ctx := context.Background()

	docker, _ := dockerpool.NewDockerClient()
	defer docker.Close()

	docker.EnsureNetwork(ctx, "network")
	docker.PullImage(ctx, "alpine:latest")

	pool, _ := dockerpool.NewDockerPool(ctx, docker, "pool", "network", dockerpool.DefaultDockerPoolConfig())
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)
	defer pool.Return(ctx, c)

	// Stream to file
	logFile, _ := os.Create("/tmp/build.log")
	defer logFile.Close()

	exitCode, err := c.ExecStream(ctx,
		[]string{"sh", "-c", "echo 'Building...' && sleep 1 && echo 'Done!'"},
		logFile,   // stdout → file
		os.Stderr, // stderr → console
		dockerpool.ExecOptions{Timeout: 30 * time.Second},
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Build finished with exit code: %d\n", exitCode)
}

// Example_execWithStdin demonstrates passing input to command
func Example_execWithStdin() {
	ctx := context.Background()

	docker, _ := dockerpool.NewDockerClient()
	defer docker.Close()

	docker.EnsureNetwork(ctx, "network")
	docker.PullImage(ctx, "alpine:latest")

	pool, _ := dockerpool.NewDockerPool(ctx, docker, "pool", "network", dockerpool.DefaultDockerPoolConfig())
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)
	defer pool.Return(ctx, c)

	// Pass input via stdin
	input := strings.NewReader("line1\nline2\nline3\n")

	var stdout bytes.Buffer
	exitCode, _ := c.ExecStream(ctx,
		[]string{"grep", "line2"},
		&stdout,
		os.Stderr,
		dockerpool.ExecOptions{Stdin: input},
	)

	fmt.Printf("Exit code: %d\n", exitCode)
	fmt.Printf("Matched: %s\n", stdout.String())
}

// Example_poolMetrics demonstrates monitoring pool state
func Example_poolMetrics() {
	ctx := context.Background()

	docker, _ := dockerpool.NewDockerClient()
	defer docker.Close()

	docker.EnsureNetwork(ctx, "network")
	docker.PullImage(ctx, "alpine:latest")

	pool, _ := dockerpool.NewDockerPool(ctx, docker, "pool", "network", dockerpool.DefaultDockerPoolConfig())
	defer pool.Shutdown(ctx)

	// Monitor pool metrics
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			fmt.Printf("Pool status: idle=%d, in_use=%d\n",
				pool.IdleCount(),
				pool.InUse(),
			)
		}
	}()

	// Simulate load
	for i := 0; i < 10; i++ {
		c, _ := pool.Acquire(ctx)
		go func(container dockerpool.Container) {
			time.Sleep(100 * time.Millisecond)
			pool.Return(ctx, container)
		}(c)
	}

	time.Sleep(2 * time.Second)
}

// Example_gracefulShutdown demonstrates proper shutdown
func Example_gracefulShutdown() {
	ctx := context.Background()

	docker, _ := dockerpool.NewDockerClient()
	defer docker.Close()

	docker.EnsureNetwork(ctx, "network")
	docker.PullImage(ctx, "alpine:latest")

	pool, _ := dockerpool.NewDockerPool(ctx, docker, "pool", "network", dockerpool.DefaultDockerPoolConfig())

	// Acquire some containers
	containers := make([]dockerpool.Container, 5)
	for i := range containers {
		containers[i], _ = pool.Acquire(ctx)
	}

	// Start shutdown in background with timeout
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	shutdownDone := make(chan error)
	go func() {
		shutdownDone <- pool.Shutdown(shutdownCtx)
	}()

	// Return all containers (required for shutdown to complete)
	for _, c := range containers {
		pool.Return(ctx, c)
	}

	// Wait for shutdown
	if err := <-shutdownDone; err != nil {
		fmt.Printf("Shutdown error: %v\n", err)
	} else {
		fmt.Println("Shutdown complete!")
	}
}

// Example_removeContainer demonstrates removing container without returning to pool
func Example_removeContainer() {
	ctx := context.Background()

	docker, _ := dockerpool.NewDockerClient()
	defer docker.Close()

	docker.EnsureNetwork(ctx, "network")
	docker.PullImage(ctx, "alpine:latest")

	pool, _ := dockerpool.NewDockerPool(ctx, docker, "pool", "network", dockerpool.DefaultDockerPoolConfig())
	defer pool.Shutdown(ctx)

	c, _ := pool.Acquire(ctx)

	// Execute potentially dangerous command
	result, err := c.Exec(ctx, []string{"rm", "-rf", "/tmp/test"})
	if err != nil || result.ExitCode != 0 {
		// Container is in bad state - remove instead of returning
		pool.Remove(ctx, c)
		return
	}

	// Container is fine - return to pool
	pool.Return(ctx, c)
}
