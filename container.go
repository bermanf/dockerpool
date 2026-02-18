package dockerpool

import (
	"context"
	"io"
	"sync"

	"github.com/moby/moby/client"
)

// Container interface for working with a container
type Container interface {
	ID() string
	Image() string
	Labels() map[string]string
	State() ContainerState
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
	Exec(ctx context.Context, cmd []string) (*ExecResult, error)
	ExecStd(ctx context.Context, cmd []string, opts ExecOptions) (*ExecResult, error)
	ExecStream(ctx context.Context, cmd []string, stdout, stderr io.Writer, opts ExecOptions) (exitCode int, err error)
}

// ContainerClient is the implementation of the Container interface
type ContainerClient struct {
	docker Docker
	id     string
	image  string
	labels map[string]string
	state  ContainerState

	rw sync.RWMutex
}
type ContainerOpts struct {
	Docker Docker
	ID     string
	Image  string
	Labels map[string]string
	State  ContainerState
}

func NewContainerClient(opts ContainerOpts) Container {
	return &ContainerClient{
		docker: opts.Docker,
		id:     opts.ID,
		image:  opts.Image,
		labels: opts.Labels,
		state:  opts.State,
	}
}

func (c *ContainerClient) ID() string {
	c.rw.RLock()
	defer c.rw.RUnlock()

	return c.id
}

func (c *ContainerClient) Image() string {
	c.rw.RLock()
	defer c.rw.RUnlock()

	return c.image
}

func (c *ContainerClient) Labels() map[string]string {
	c.rw.RLock()
	defer c.rw.RUnlock()

	return c.labels
}

func (c *ContainerClient) State() ContainerState {
	c.rw.RLock()
	defer c.rw.RUnlock()

	return c.state
}

func (c *ContainerClient) Stop(ctx context.Context) error {
	err := c.docker.StopContainer(ctx, c.id, client.ContainerStopOptions{})
	if err != nil {
		return err
	}
	c.rw.Lock()
	c.state = StateExited
	c.rw.Unlock()

	return nil
}

func (c *ContainerClient) Start(ctx context.Context) error {
	err := c.docker.StartContainer(ctx, c.id, client.ContainerStartOptions{})
	if err != nil {
		return err
	}
	c.rw.Lock()
	c.state = StateRunning
	c.rw.Unlock()

	return nil
}

func (c *ContainerClient) Exec(ctx context.Context, cmd []string) (*ExecResult, error) {
	return c.docker.Exec(ctx, c.id, cmd)
}

func (c *ContainerClient) ExecStd(ctx context.Context, cmd []string, opts ExecOptions) (*ExecResult, error) {
	return c.docker.ExecStd(ctx, c.id, cmd, opts)
}

func (c *ContainerClient) ExecStream(ctx context.Context, cmd []string, stdout, stderr io.Writer, opts ExecOptions) (int, error) {
	return c.docker.ExecStream(ctx, c.id, cmd, stdout, stderr, opts)
}
