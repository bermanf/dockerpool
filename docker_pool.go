package dockerpool

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/types/container"
	"golang.org/x/sync/errgroup"
)

// Config contains all configuration for creating a DockerPool.
type Config struct {
	// Required
	Name    string        // pool name (used for container labels)
	Network NetworkConfig // docker network configuration
	Image   ImageConfig   // container image configuration

	// Optional - have sensible defaults
	Cmd                    []string      // container command (default: ["sleep", "infinity"])
	MinIdle                int           // minimum idle containers (default: 5)
	MaxIdle                int           // maximum idle containers (default: 10)
	RefillInterval         time.Duration // pool refill check interval (default: 100ms)
	MaxConcurrentDockerOps int           // max parallel docker operations (default: 10)
	MaxConcurrentAcquire   int           // max parallel container acquisition (default: 10)
	MaxRefillWorkers       int           // max parallel container creation goroutines (default: 5)

	// Advanced - for custom container configuration
	ContainerConfig *container.Config     // custom container config (overrides Image, Cmd)
	HostConfig      *container.HostConfig // custom host config (volumes, resources, etc.)
	Labels          map[string]string     // additional container labels

	// Callbacks
	OnError func(error) // error handler (default: log.Printf)
}

type NetworkConfig struct {
	Name        string
	Driver      string
	Labels      map[string]string
	NeedsCreate bool
	NeedsRemove bool
}

type ImageConfig struct {
	Name      string
	NeedsPull bool
}

// DockerPool manages a pool of Docker containers.
type DockerPool struct {
	docker Docker
	stack  Stack[Container]
	config Config

	containerConfig CreateContainerOptions
	inUse           atomic.Int64
	minIdle         int
	maxIdle         int

	refillInterval         time.Duration
	maxConcurrentDockerOps int
	maxConcurrentAcquire   int

	onError        func(error)
	cancel         context.CancelFunc
	watcherDone    chan struct{}
	released       chan struct{}
	acquireLimiter chan struct{}
	watcherStarted atomic.Bool
	ownsDocker     bool

	shutdown atomic.Bool    // true after Shutdown is called
	removeWg sync.WaitGroup // tracks in-flight removeContainer goroutines
}

// New creates a new DockerPool.
// This is the main entry point for users.
func New(ctx context.Context, cfg Config) (*DockerPool, error) {
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	docker, err := newDockerClient()
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	// Ensure network exists
	if cfg.Network.NeedsCreate {
		if _, err := docker.EnsureNetwork(ctx, cfg.Network.Name, cfg.Network.Driver, cfg.Network.Labels); err != nil {
			docker.Close()
			return nil, fmt.Errorf("ensure network: %w", err)
		}
	}
	// Pull image if requested
	if cfg.Image.NeedsPull {
		if err := docker.PullImage(ctx, cfg.Image.Name); err != nil {
			docker.Close()
			return nil, fmt.Errorf("pull image: %w", err)
		}
	}

	pool, err := newDockerPool(ctx, docker, cfg)
	if err != nil {
		docker.Close()
		return nil, err
	}

	pool.ownsDocker = true
	return pool, nil
}

// NewWithDocker creates a pool with a provided Docker implementation.
// Use this for testing with mocks or custom Docker clients.
func NewWithDocker(ctx context.Context, docker Docker, cfg Config) (*DockerPool, error) {
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	return newDockerPool(ctx, docker, cfg)
}

func validateConfig(cfg *Config) error {
	if cfg.Name == "" {
		return ErrPoolNameRequired
	}
	if cfg.Network.Name == "" {
		return ErrNetworkNameRequired
	}
	if cfg.Image.Name == "" {
		return ErrImageRequired
	}
	return nil
}

func newDockerPool(ctx context.Context, docker Docker, cfg Config) (*DockerPool, error) {
	p := &DockerPool{
		docker: docker,
		stack:  NewStack[Container](),
		config: cfg,

		released: make(chan struct{}),
	}

	applyConfig(p, cfg)

	// Initialize acquire limiter (limits concurrent container creation when pool is empty)
	p.acquireLimiter = make(chan struct{}, p.maxConcurrentAcquire)

	// Add pool labels
	p.containerConfig.Config.Labels = MergeLabels(
		p.containerConfig.Config.Labels,
		PoolLabels(cfg.Name),
	)

	// Auto-start watcher
	if err := p.startWatcher(ctx); err != nil {
		return nil, fmt.Errorf("start watcher: %w", err)
	}

	p.watcherStarted.Store(true)
	return p, nil
}

func applyConfig(p *DockerPool, cfg Config) {
	// Defaults
	if cfg.OnError == nil {
		cfg.OnError = func(err error) {
			log.Printf("dockerpool error: %v", err)
		}
	}
	if cfg.MinIdle == 0 {
		cfg.MinIdle = 5
	}
	if cfg.MaxIdle == 0 {
		cfg.MaxIdle = 10
	}
	if cfg.RefillInterval == 0 {
		cfg.RefillInterval = 100 * time.Millisecond
	}
	if cfg.MaxConcurrentDockerOps == 0 {
		cfg.MaxConcurrentDockerOps = 10
	}
	if cfg.MaxConcurrentAcquire == 0 {
		cfg.MaxConcurrentAcquire = 10
	}
	if cfg.MaxRefillWorkers == 0 {
		cfg.MaxRefillWorkers = 5
	}
	if len(cfg.Cmd) == 0 {
		cfg.Cmd = []string{"sleep", "infinity"}
	}

	// Build container config
	if cfg.ContainerConfig != nil {
		p.containerConfig.Config = cfg.ContainerConfig
	} else {
		p.containerConfig.Config = &container.Config{
			Image: cfg.Image.Name,
			Cmd:   cfg.Cmd,
		}
	}

	if cfg.HostConfig != nil {
		p.containerConfig.HostConfig = cfg.HostConfig
	} else {
		p.containerConfig.HostConfig = &container.HostConfig{}
	}

	// Merge custom labels
	if cfg.Labels != nil {
		p.containerConfig.Config.Labels = MergeLabels(
			p.containerConfig.Config.Labels,
			cfg.Labels,
		)
	}

	// Apply to pool
	p.onError = cfg.OnError
	p.minIdle = cfg.MinIdle
	p.maxIdle = cfg.MaxIdle
	p.refillInterval = cfg.RefillInterval
	p.maxConcurrentDockerOps = cfg.MaxConcurrentDockerOps
	p.maxConcurrentAcquire = cfg.MaxConcurrentAcquire
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
	if err := p.refill(ctx); err != nil {
		return fmt.Errorf("initial refill: %w", err)
	}

	go func() {
		ticker := time.NewTicker(p.refillInterval)
		defer ticker.Stop()
		defer close(p.watcherDone)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.refill(ctx); err != nil {
					p.onError(fmt.Errorf("refill: %w", err))
				}
			}
		}
	}()
	return nil
}

// refill refills the pool to minIdle
func (p *DockerPool) refill(ctx context.Context) error {
	available := p.stack.Len()
	desired := p.minIdle - available

	dockerOpsLimiter := make(chan struct{}, p.maxConcurrentDockerOps)
	defer close(dockerOpsLimiter)
	eg := errgroup.Group{}
	eg.SetLimit(p.maxConcurrentDockerOps)

	for range desired {
		if ctx.Err() != nil {
			break
		}

		eg.Go(func() error {
			c, err := p.docker.CreateContainer(ctx, p.config.Network.Name, p.containerConfig)
			if err == nil {
				p.stack.Push(c)
			}

			return err
		})
	}

	return eg.Wait()
}

func (p *DockerPool) syncDockerPool(ctx context.Context) error {
	containers, err := p.docker.ListContainersByLabels(ctx,
		Label{Key: LabelManagedBy, Value: LabelManagedByValue},
		Label{Key: LabelPoolName, Value: p.config.Name},
	)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	for _, container := range containers {
		p.stack.Push(container)
	}
	return nil
}

// Acquire gets a container from the pool or creates a new one.
// After use, you must call Return() or Remove().
// Returns ErrPoolShutdown if the pool has been shut down.
func (p *DockerPool) Acquire(ctx context.Context) (Container, error) {
	if p.shutdown.Load() {
		return nil, ErrPoolShutdown
	}

	if c, ok := p.stack.Pop(); ok {
		p.inUse.Add(1)
		return c, nil
	}

	// Check again after pool was empty (shutdown might have started)
	if p.shutdown.Load() {
		return nil, ErrPoolShutdown
	}

	select {
	case p.acquireLimiter <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-p.acquireLimiter }()

	// Pool is empty — create container synchronously
	container, err := p.docker.CreateContainer(ctx, p.config.Network.Name, p.containerConfig)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	p.inUse.Add(1)
	return container, nil
}

// Shutdown gracefully shuts down the pool.
// It waits for all in-use containers to be returned, then removes all containers.
func (p *DockerPool) Shutdown(ctx context.Context) error {
	p.shutdown.Store(true)

	if p.cancel != nil {
		p.cancel()
	}
	if p.watcherStarted.Load() {
		select {
		case <-p.watcherDone:
		case <-ctx.Done():
			return fmt.Errorf("wait for watcher: %w", ctx.Err())
		}
	}

	// Wait for all containers to be returned
	for p.inUse.Load() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for containers: %w", ctx.Err())
		case <-p.released:
		}
	}

	// Wait for any in-flight removeContainer goroutines from Return/Remove
	p.removeWg.Wait()

	dockerOpsLimiter := make(chan struct{}, p.maxConcurrentDockerOps)
	var wg sync.WaitGroup

	for {
		c, ok := p.stack.Pop()
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

			// Direct removal without containerReleased (shutdown is true)
			if err := p.docker.RemoveContainer(ctx, container.ID()); err != nil {
				p.onError(fmt.Errorf("remove container: %w", err))
			}
		}(c)
	}

	wg.Wait()

	if p.config.Network.NeedsRemove {
		if err := p.docker.RemoveNetwork(ctx, p.config.Network.Name); err != nil {
			return fmt.Errorf("remove network: %w", err)
		}
	}
	if p.ownsDocker {
		if err := p.docker.Close(); err != nil {
			return fmt.Errorf("close docker client: %w", err)
		}
	}

	return nil
}

// InUse returns the number of containers currently in use.
func (p *DockerPool) InUse() int64 {
	return p.inUse.Load()
}

// IdleCount returns the number of idle containers in the pool.
func (p *DockerPool) IdleCount() int {
	return p.stack.Len()
}

// Return returns the container to the pool.
// If the pool is full (>= maxIdle) or shutdown is in progress, the container is removed.
func (p *DockerPool) Return(ctx context.Context, c Container) {
	p.inUse.Add(-1)

	if p.shutdown.Load() || p.stack.Len() >= p.maxIdle {
		p.removeWg.Add(1)
		go func() {
			defer p.removeWg.Done()
			p.removeContainer(ctx, c)
		}()
		return
	}
	p.stack.Push(c)
	p.containerReleased()
}

// Remove removes the container (does not return it to the pool).
func (p *DockerPool) Remove(ctx context.Context, c Container) {
	p.inUse.Add(-1)
	p.removeWg.Add(1)
	go func() {
		defer p.removeWg.Done()
		p.removeContainer(ctx, c)
	}()
}

func (p *DockerPool) removeContainer(ctx context.Context, c Container) {
	if err := p.docker.RemoveContainer(ctx, c.ID()); err != nil {
		p.onError(fmt.Errorf("remove container: %w", err))
	}
	p.containerReleased()
}

func (p *DockerPool) containerReleased() {
	select {
	case p.released <- struct{}{}:
	default:
	}
}
