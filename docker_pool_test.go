package dockerpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockDocker is a mock implementation of the Docker interface
type MockDocker struct {
	mu sync.Mutex

	PingFunc                   func(ctx context.Context) error
	CreateContainerFunc        func(ctx context.Context, networkName string, opts CreateContainerOptions) (Container, error)
	ListContainersByLabelsFunc func(ctx context.Context, labels ...Label) ([]Container, error)
	RemoveContainerFunc        func(ctx context.Context, containerID string) error

	CreateContainerCalls int
	RemoveContainerCalls int
}

func (m *MockDocker) GetRemoveContainerCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.RemoveContainerCalls
}

func (m *MockDocker) GetCreateContainerCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.CreateContainerCalls
}

func (m *MockDocker) Close() error { return nil }

func (m *MockDocker) Ping(ctx context.Context) error {
	if m.PingFunc != nil {
		return m.PingFunc(ctx)
	}
	return nil
}

func (m *MockDocker) SetCleanupTimeout(timeout time.Duration) {}

func (m *MockDocker) PullImage(ctx context.Context, image string) error { return nil }

func (m *MockDocker) EnsureNetwork(ctx context.Context, networkName string, driver string, labels map[string]string) (bool, error) {
	return false, nil
}

func (m *MockDocker) StartContainer(ctx context.Context, containerID string, opts client.ContainerStartOptions) error {
	return nil
}

func (m *MockDocker) RemoveNetwork(ctx context.Context, networkName string) error { return nil }

func (m *MockDocker) CreateContainer(ctx context.Context, networkName string, opts CreateContainerOptions) (Container, error) {
	m.mu.Lock()
	m.CreateContainerCalls++
	m.mu.Unlock()

	if m.CreateContainerFunc != nil {
		return m.CreateContainerFunc(ctx, networkName, opts)
	}
	return &MockContainer{id: "test-container", state: StateRunning}, nil
}

func (m *MockDocker) StopContainer(ctx context.Context, containerID string, opts client.ContainerStopOptions) error {
	return nil
}

func (m *MockDocker) RemoveContainer(ctx context.Context, containerID string) error {
	m.mu.Lock()
	m.RemoveContainerCalls++
	m.mu.Unlock()

	if m.RemoveContainerFunc != nil {
		return m.RemoveContainerFunc(ctx, containerID)
	}
	return nil
}

func (m *MockDocker) Exec(ctx context.Context, containerID string, cmd []string) (*ExecResult, error) {
	return &ExecResult{}, nil
}

func (m *MockDocker) ExecStd(ctx context.Context, containerID string, cmd []string, opts ExecOptions) (*ExecResult, error) {
	return &ExecResult{}, nil
}

func (m *MockDocker) ExecStream(ctx context.Context, containerID string, cmd []string, stdout, stderr io.Writer, opts ExecOptions) (int, error) {
	return 0, nil
}

func (m *MockDocker) ListContainersByLabels(ctx context.Context, labels ...Label) ([]Container, error) {
	if m.ListContainersByLabelsFunc != nil {
		return m.ListContainersByLabelsFunc(ctx, labels...)
	}
	return nil, nil
}

// MockContainer is a mock implementation of the Container interface
type MockContainer struct {
	id     string
	image  string
	labels map[string]string
	state  ContainerState
}

func (m *MockContainer) ID() string                      { return m.id }
func (m *MockContainer) Image() string                   { return m.image }
func (m *MockContainer) Labels() map[string]string       { return m.labels }
func (m *MockContainer) State() ContainerState           { return m.state }
func (m *MockContainer) Stop(ctx context.Context) error  { return nil }
func (m *MockContainer) Start(ctx context.Context) error { return nil }

func (m *MockContainer) Exec(ctx context.Context, cmd []string) (*ExecResult, error) {
	return &ExecResult{}, nil
}

func (m *MockContainer) ExecStd(ctx context.Context, cmd []string, opts ExecOptions) (*ExecResult, error) {
	return &ExecResult{}, nil
}

func (m *MockContainer) ExecStream(ctx context.Context, cmd []string, stdout, stderr io.Writer, opts ExecOptions) (int, error) {
	return 0, nil
}

// Helper to create test config
func testConfig(name, network, image string) Config {
	return Config{
		Name:    name,
		Network: NetworkConfig{Name: network},
		Image:   ImageConfig{Name: image},
	}
}

func testConfigWithIdle(name, network, image string, minIdle, maxIdle int) Config {
	cfg := testConfig(name, network, image)
	cfg.MinIdle = minIdle
	cfg.MaxIdle = maxIdle
	return cfg
}

func TestNewDockerPool(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		wantErr     error
		wantMinIdle int
		wantMaxIdle int
	}{
		{
			name:    "empty name returns error",
			config:  testConfig("", "test-network", "alpine:latest"),
			wantErr: ErrPoolNameRequired,
		},
		{
			name:    "empty network returns error",
			config:  testConfig("test-pool", "", "alpine:latest"),
			wantErr: ErrNetworkNameRequired,
		},
		{
			name:    "empty image returns error",
			config:  testConfig("test-pool", "test-network", ""),
			wantErr: ErrImageRequired,
		},
		{
			name:        "valid config creates pool with defaults",
			config:      testConfig("test-pool", "test-network", "alpine:latest"),
			wantMinIdle: 5,
			wantMaxIdle: 10,
		},
		{
			name:        "with custom min/max idle",
			config:      testConfigWithIdle("test-pool", "test-network", "alpine:latest", 3, 6),
			wantMinIdle: 3,
			wantMaxIdle: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &MockDocker{}
			pool, err := NewWithDocker(context.Background(), mock, tt.config)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)

			// Cleanup watcher for successful tests
			defer func() {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer shutdownCancel()
				pool.Shutdown(shutdownCtx)
			}()

			assert.Equal(t, tt.wantMinIdle, pool.minIdle)
			assert.Equal(t, tt.wantMaxIdle, pool.maxIdle)
			assert.Equal(t, tt.config.Name, pool.containerConfig.Config.Labels[LabelPoolName])
		})
	}
}

func TestPoolInitialization(t *testing.T) {
	tests := []struct {
		name      string
		setupMock func(*MockDocker)
		wantErr   bool
		errMsg    string
	}{
		{
			name: "ping failure",
			setupMock: func(m *MockDocker) {
				m.PingFunc = func(ctx context.Context) error {
					return errors.New("connection refused")
				}
			},
			wantErr: true,
			errMsg:  "ping docker",
		},
		{
			name: "list containers failure",
			setupMock: func(m *MockDocker) {
				m.ListContainersByLabelsFunc = func(ctx context.Context, labels ...Label) ([]Container, error) {
					return nil, errors.New("list error")
				}
			},
			wantErr: true,
			errMsg:  "sync pool",
		},
		{
			name: "success with existing containers",
			setupMock: func(m *MockDocker) {
				m.ListContainersByLabelsFunc = func(ctx context.Context, labels ...Label) ([]Container, error) {
					return []Container{
						&MockContainer{id: "existing-1", state: StateRunning},
						&MockContainer{id: "existing-2", state: StateRunning},
					}, nil
				}
			},
			wantErr: false,
		},
		{
			name:      "success empty pool",
			setupMock: func(m *MockDocker) {},
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &MockDocker{}
			if tt.setupMock != nil {
				tt.setupMock(mock)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			pool, err := NewWithDocker(ctx, mock, testConfigWithIdle("test-pool", "test-network", "alpine:latest", 2, 5))

			if tt.wantErr {
				require.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
				return
			}

			require.NoError(t, err)

			// Cleanup: stop watcher for successful tests
			defer func() {
				if pool != nil {
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer shutdownCancel()
					pool.Shutdown(shutdownCtx)
				}
			}()
		})
	}
}

func TestAcquire(t *testing.T) {
	tests := []struct {
		name            string
		setupPool       func(*DockerPool)
		setupMock       func(*MockDocker)
		wantErr         bool
		wantContainerID string
	}{
		{
			name: "acquire from pool",
			setupPool: func(p *DockerPool) {
				p.stack.Push(&MockContainer{id: "pooled-container", state: StateRunning})
			},
			wantContainerID: "pooled-container",
		},
		{
			name: "create new when pool empty",
			setupMock: func(m *MockDocker) {
				m.CreateContainerFunc = func(ctx context.Context, networkName string, opts CreateContainerOptions) (Container, error) {
					return &MockContainer{id: "new-container", state: StateRunning}, nil
				}
			},
			wantContainerID: "new-container",
		},
		{
			name: "create container fails",
			setupMock: func(m *MockDocker) {
				var callCount atomic.Int32
				m.CreateContainerFunc = func(ctx context.Context, networkName string, opts CreateContainerOptions) (Container, error) {
					n := callCount.Add(1)
					if n <= 2 {
						return &MockContainer{id: fmt.Sprintf("init-%d", n), state: StateRunning}, nil
					}
					return nil, errors.New("create failed")
				}
			},
			setupPool: func(p *DockerPool) {
				// Drain the pool so Acquire has to create a new container
				for p.stack.Len() > 0 {
					p.stack.Pop()
				}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &MockDocker{}
			if tt.setupMock != nil {
				tt.setupMock(mock)
			}

			pool, err := NewWithDocker(context.Background(), mock, testConfigWithIdle("test-pool", "test-network", "alpine:latest", 2, 5))
			require.NoError(t, err)

			// Cleanup watcher
			defer func() {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer shutdownCancel()
				pool.Shutdown(shutdownCtx)
			}()

			if tt.setupPool != nil {
				tt.setupPool(pool)
			}

			ctx := context.Background()
			container, err := pool.Acquire(ctx)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantContainerID, container.ID())
			assert.Equal(t, int64(1), pool.InUse())
		})
	}
}

func TestReturn(t *testing.T) {
	t.Run("return to pool when not full", func(t *testing.T) {
		mock := &MockDocker{}
		pool, err := NewWithDocker(context.Background(), mock, testConfigWithIdle("test-pool", "test-network", "alpine:latest", 2, 5))
		require.NoError(t, err)

		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer shutdownCancel()
			pool.Shutdown(shutdownCtx)
		}()

		initialSize := pool.IdleCount() // Pool already has minIdle containers
		pool.inUse.Store(1)
		container := &MockContainer{id: "returned-container", state: StateRunning}

		pool.Return(context.Background(), container)
		time.Sleep(10 * time.Millisecond)

		assert.Equal(t, initialSize+1, pool.IdleCount()) // One more added
		assert.Equal(t, 0, mock.GetRemoveContainerCalls())
		assert.Equal(t, int64(0), pool.InUse())
	})

	t.Run("remove when pool is full", func(t *testing.T) {
		mock := &MockDocker{}
		pool, err := NewWithDocker(context.Background(), mock, testConfigWithIdle("test-pool", "test-network", "alpine:latest", 2, 5))
		require.NoError(t, err)

		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer shutdownCancel()
			pool.Shutdown(shutdownCtx)
		}()

		// Fill pool to maxIdle
		for pool.IdleCount() < 5 {
			pool.stack.Push(&MockContainer{id: "c"})
		}

		initialSize := pool.IdleCount()
		pool.inUse.Store(1)
		container := &MockContainer{id: "returned-container", state: StateRunning}

		pool.Return(context.Background(), container)
		time.Sleep(10 * time.Millisecond)

		assert.Equal(t, initialSize, pool.IdleCount())     // Size unchanged
		assert.Equal(t, 1, mock.GetRemoveContainerCalls()) // Container removed
		assert.Equal(t, int64(0), pool.InUse())
	})
}

func TestRemove(t *testing.T) {
	mock := &MockDocker{}
	pool, err := NewWithDocker(context.Background(), mock, testConfig("test-pool", "test-network", "alpine:latest"))
	require.NoError(t, err)

	// Cleanup watcher
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer shutdownCancel()
		pool.Shutdown(shutdownCtx)
	}()

	initialSize := pool.IdleCount()
	initialRemoveCalls := mock.GetRemoveContainerCalls()

	pool.inUse.Store(1)
	container := &MockContainer{id: "to-remove", state: StateRunning}

	pool.Remove(context.Background(), container)

	// Give time for async removal
	time.Sleep(10 * time.Millisecond)

	// Pool size should not change (Remove doesn't return to pool)
	assert.Equal(t, initialSize, pool.IdleCount())
	// One more remove call
	assert.Equal(t, initialRemoveCalls+1, mock.GetRemoveContainerCalls())
	assert.Equal(t, int64(0), pool.InUse())
}

func TestShutdown(t *testing.T) {
	tests := []struct {
		name       string
		setupPool  func(*DockerPool)
		ctxTimeout time.Duration
		wantErr    bool
		wantEmpty  bool
	}{
		{
			name:       "shutdown empty pool",
			setupPool:  func(p *DockerPool) {},
			ctxTimeout: time.Second,
			wantEmpty:  true,
		},
		{
			name: "shutdown with containers in pool",
			setupPool: func(p *DockerPool) {
				p.stack.Push(&MockContainer{id: "c1"})
				p.stack.Push(&MockContainer{id: "c2"})
			},
			ctxTimeout: time.Second,
			wantEmpty:  true,
		},
		{
			name: "shutdown waits for in-use containers",
			setupPool: func(p *DockerPool) {
				p.inUse.Store(1)
				go func() {
					time.Sleep(50 * time.Millisecond)
					p.inUse.Store(0)
					p.containerReleased()
				}()
			},
			ctxTimeout: time.Second,
			wantEmpty:  true,
		},
		{
			name: "shutdown timeout waiting for containers",
			setupPool: func(p *DockerPool) {
				p.inUse.Store(1) // Never returned
			},
			ctxTimeout: 150 * time.Millisecond,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &MockDocker{}

			pool, err := NewWithDocker(context.Background(), mock, testConfigWithIdle("test-pool", "test-network", "alpine:latest", 2, 5))
			require.NoError(t, err)

			if tt.setupPool != nil {
				tt.setupPool(pool)
			}

			ctx, cancel := context.WithTimeout(context.Background(), tt.ctxTimeout)
			defer cancel()

			err = pool.Shutdown(ctx)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)

			if tt.wantEmpty {
				assert.Equal(t, 0, pool.IdleCount())
			}
		})
	}
}

func TestWithContainerConfig(t *testing.T) {
	t.Run("custom image from config", func(t *testing.T) {
		mock := &MockDocker{}
		cfg := testConfig("test-pool", "test-network", "python:3.11")
		pool, err := NewWithDocker(context.Background(), mock, cfg)
		require.NoError(t, err)

		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer shutdownCancel()
			pool.Shutdown(shutdownCtx)
		}()

		assert.Equal(t, "python:3.11", pool.containerConfig.Config.Image)
	})

	t.Run("custom container config overrides image", func(t *testing.T) {
		mock := &MockDocker{}
		cfg := testConfig("test-pool", "test-network", "alpine:latest")
		cfg.ContainerConfig = &container.Config{
			Image: "custom:image",
			Cmd:   []string{"custom", "cmd"},
		}
		pool, err := NewWithDocker(context.Background(), mock, cfg)
		require.NoError(t, err)

		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer shutdownCancel()
			pool.Shutdown(shutdownCtx)
		}()

		assert.Equal(t, "custom:image", pool.containerConfig.Config.Image)
	})
}

func TestWithLabels(t *testing.T) {
	mock := &MockDocker{}
	cfg := testConfig("test-pool", "test-network", "alpine:latest")
	cfg.Labels = map[string]string{"env": "test", "app": "myapp"}

	pool, err := NewWithDocker(context.Background(), mock, cfg)
	require.NoError(t, err)

	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer shutdownCancel()
		pool.Shutdown(shutdownCtx)
	}()

	wantLabels := map[string]string{
		"env":          "test",
		"app":          "myapp",
		LabelManagedBy: LabelManagedByValue,
		LabelPoolName:  "test-pool",
	}

	for k, v := range wantLabels {
		assert.Equal(t, v, pool.containerConfig.Config.Labels[k], "label %q mismatch", k)
	}
}

func TestSyncDockerPool(t *testing.T) {
	t.Run("syncs existing containers on startup", func(t *testing.T) {
		mock := &MockDocker{}
		mock.ListContainersByLabelsFunc = func(ctx context.Context, labels ...Label) ([]Container, error) {
			return []Container{
				&MockContainer{id: "c1", state: StateRunning},
				&MockContainer{id: "c2", state: StateRunning},
				&MockContainer{id: "c3", state: StateRunning},
			}, nil
		}

		// minIdle=2 but we already have 3 containers from sync
		pool, err := NewWithDocker(context.Background(), mock, testConfigWithIdle("test-pool", "test-network", "alpine:latest", 2, 5))
		require.NoError(t, err)

		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer shutdownCancel()
			pool.Shutdown(shutdownCtx)
		}()

		// Pool should have at least 3 containers from sync
		assert.GreaterOrEqual(t, pool.IdleCount(), 3)
		// No new containers should be created since we have more than minIdle
		assert.Equal(t, 0, mock.GetCreateContainerCalls())
	})

	t.Run("refills if synced containers less than minIdle", func(t *testing.T) {
		mock := &MockDocker{}
		mock.ListContainersByLabelsFunc = func(ctx context.Context, labels ...Label) ([]Container, error) {
			return []Container{
				&MockContainer{id: "c1", state: StateRunning},
			}, nil
		}

		// minIdle=3 but we only have 1 container from sync
		pool, err := NewWithDocker(context.Background(), mock, testConfigWithIdle("test-pool", "test-network", "alpine:latest", 3, 5))
		require.NoError(t, err)

		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer shutdownCancel()
			pool.Shutdown(shutdownCtx)
		}()

		// Should have 3 containers (1 synced + 2 created)
		assert.Equal(t, 3, pool.IdleCount())
		assert.Equal(t, 2, mock.GetCreateContainerCalls())
	})

	t.Run("list error fails pool creation", func(t *testing.T) {
		mock := &MockDocker{}
		mock.ListContainersByLabelsFunc = func(ctx context.Context, labels ...Label) ([]Container, error) {
			return nil, errors.New("docker error")
		}

		_, err := NewWithDocker(context.Background(), mock, testConfig("test-pool", "test-network", "alpine:latest"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sync pool")
	})
}
