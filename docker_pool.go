package dockerpool

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/types/container"
)

type DockerPool struct {
	docker          Docker
	pool            Pool[Container]
	name            string
	networkName     string
	containerConfig CreateContainerOptions

	inUse   atomic.Int64 // containers currently in use
	minIdle int          // minimum idle containers in the pool
	maxIdle int          // maximum idle containers (excess are removed on Return)

	refillInterval         time.Duration // pool refill interval
	maxConcurrentDockerOps int           // maximum concurrent Docker operations

	onError        func(error)
	cancel         context.CancelFunc
	watcherDone    chan struct{}
	watcherStarted atomic.Bool
}

func NewDockerPool(ctx context.Context, docker Docker, poolName string, networkName string, config DockerPoolConfig) (*DockerPool, error) {
	if poolName == "" {
		return nil, ErrPoolNameRequired
	}
	if networkName == "" {
		return nil, ErrNetworkNameRequired
	}
	p := &DockerPool{
		docker:      docker,
		name:        poolName,
		networkName: networkName,
		pool:        NewPool[Container](),
	}

	applyConfig(p, config)

	p.containerConfig.Config.Labels = MergeLabels(p.containerConfig.Config.Labels, PoolLabels(poolName))

	// Auto-start watcher
	if err := p.startWatcher(ctx); err != nil {
		return nil, fmt.Errorf("start watcher: %w", err)
	}

	p.watcherStarted.Store(true)

	return p, nil
}

type DockerPoolConfig struct {
	// Error handling
	OnError func(error)

	// Container configuration
	ContainerConfig CreateContainerOptions
	Labels          map[string]string

	// Pool sizing
	MinIdle int
	MaxIdle int

	// Timing
	RefillInterval time.Duration

	// Concurrency limits
	MaxConcurrentDockerOps int
}

// DefaultDockerPoolConfig returns a config with sensible defaults
func DefaultDockerPoolConfig() DockerPoolConfig {
	return DockerPoolConfig{
		OnError: func(err error) {
			log.Printf("error: %v", err)
		},
		ContainerConfig: CreateContainerOptions{
			Config: &container.Config{
				Image: "alpine:latest",
				Cmd:   []string{"sleep", "infinity"},
			},
			HostConfig: &container.HostConfig{},
		},
		MinIdle:                5,
		MaxIdle:                10,
		RefillInterval:         100 * time.Millisecond,
		MaxConcurrentDockerOps: 10,
	}
}

func applyConfig(p *DockerPool, config DockerPoolConfig) {
	// Apply defaults
	if config.OnError == nil {
		config.OnError = func(err error) {
			log.Printf("error: %v", err)
		}
	}
	if config.ContainerConfig.Config == nil {
		config.ContainerConfig = CreateContainerOptions{
			Config: &container.Config{
				Image: "alpine:latest",
				Cmd:   []string{"sleep", "infinity"},
			},
			HostConfig: &container.HostConfig{},
		}
	}
	if config.MinIdle == 0 {
		config.MinIdle = 5
	}
	if config.MaxIdle == 0 {
		config.MaxIdle = 10
	}
	if config.RefillInterval == 0 {
		config.RefillInterval = 100 * time.Millisecond
	}
	if config.MaxConcurrentDockerOps == 0 {
		config.MaxConcurrentDockerOps = 10
	}

	// Apply config to pool
	p.onError = config.OnError
	p.containerConfig = config.ContainerConfig
	p.minIdle = config.MinIdle
	p.maxIdle = config.MaxIdle
	p.refillInterval = config.RefillInterval
	p.maxConcurrentDockerOps = config.MaxConcurrentDockerOps

	// Merge labels
	if config.Labels != nil {
		p.containerConfig.Config.Labels = MergeLabels(p.containerConfig.Config.Labels, config.Labels)
	}
}

func (p *DockerPool) startWatcher(ctx context.Context) error {
	if err := p.docker.Ping(ctx); err != nil {
		return fmt.Errorf("ping docker: %w", err)
	}

	if err := p.syncDockerPool(ctx); err != nil {
		return fmt.Errorf("sync pool: %w", err)
	}

	ctx, p.cancel = context.WithCancel(context.Background())
	p.watcherDone = make(chan struct{})

	// Initial fill to minIdle
	p.refill(ctx)

	go func() {
		ticker := time.NewTicker(p.refillInterval)
		defer ticker.Stop()
		defer close(p.watcherDone)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.refill(ctx)
			}
		}
	}()
	return nil
}

// refill refills the pool to minIdle
func (p *DockerPool) refill(ctx context.Context) {
	idle := p.pool.Len()
	needed := p.minIdle - idle

	dockerOpsLimiter := make(chan struct{}, p.maxConcurrentDockerOps)
	var wg sync.WaitGroup

	for i := 0; i < needed; i++ {
		select {
		case <-ctx.Done():
			return
		case dockerOpsLimiter <- struct{}{}:
		}

		wg.Add(1)
		go func() {
			defer func() { <-dockerOpsLimiter }()
			defer wg.Done()
			c, err := p.docker.CreateContainer(ctx, p.networkName, p.containerConfig)
			if err != nil {
				p.onError(fmt.Errorf("refill: %w", err))
				return
			}
			p.pool.Push(c)
		}()
	}

	wg.Wait()
}

func (p *DockerPool) syncDockerPool(ctx context.Context) error {
	containers, err := p.docker.ListContainersByLabels(ctx,
		Label{Key: LabelManagedBy, Value: LabelManagedByValue},
		Label{Key: LabelPoolName, Value: p.name},
	)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	for _, container := range containers {
		p.pool.Push(container)
	}
	return nil
}

// Acquire gets a container from the pool or creates a new one.
// After use, you must call Return() or Remove().
func (p *DockerPool) Acquire(ctx context.Context) (Container, error) {
	if c, ok := p.pool.Pop(); ok {
		p.inUse.Add(1)
		return c, nil
	}

	// Pool is empty — create container synchronously
	container, err := p.docker.CreateContainer(ctx, p.networkName, p.containerConfig)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	p.inUse.Add(1)
	return container, nil
}

func (p *DockerPool) Shutdown(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.watcherStarted.Load() {
		select {
		case <-p.watcherDone:
		case <-ctx.Done():
			return fmt.Errorf("wait for replenisher: %w", ctx.Err())
		}
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	// Wait for all containers to be returned
	for p.inUse.Load() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for containers: %w", ctx.Err())
		case <-ticker.C:
		}
	}

	// Remove all containers from pool
	dockerOpsLimiter := make(chan struct{}, p.maxConcurrentDockerOps)
	var wg sync.WaitGroup

	for {
		c, ok := p.pool.Pop()
		if !ok {
			break
		}

		wg.Add(1)
		go func(container Container) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				return
			case dockerOpsLimiter <- struct{}{}:
				defer func() { <-dockerOpsLimiter }()
			}

			p.removeContainer(ctx, container)
		}(c)
	}

	wg.Wait()
	return nil
}

// InUse returns the number of containers currently in use
func (p *DockerPool) InUse() int64 {
	return p.inUse.Load()
}

// IdleCount returns the number of idle containers in the pool
func (p *DockerPool) IdleCount() int {
	return p.pool.Len()
}

// Return returns the container to the pool.
// If the pool is full (>= maxIdle), the container is removed.
func (p *DockerPool) Return(ctx context.Context, c Container) {
	p.inUse.Add(-1)

	// If pool is already full — remove the container
	if p.pool.Len() >= p.maxIdle {
		go p.removeContainer(ctx, c)
		return
	}
	p.pool.Push(c)
}

// Remove removes the container (does not return it to the pool)
func (p *DockerPool) Remove(ctx context.Context, c Container) {
	p.inUse.Add(-1)
	go p.removeContainer(ctx, c)
}

func (p *DockerPool) removeContainer(ctx context.Context, c Container) {
	if err := p.docker.RemoveContainer(ctx, c.ID()); err != nil {
		p.onError(fmt.Errorf("remove container: %w", err))
	}
}
