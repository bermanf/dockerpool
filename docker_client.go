package dockerpool

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Docker is the interface for working with Docker.
type Docker interface {
	// Close closes the connection to Docker
	Close() error

	// Ping checks connection to Docker daemon
	Ping(ctx context.Context) error

	// Image operations
	PullImage(ctx context.Context, image string) error

	// Network operations
	EnsureNetwork(ctx context.Context, networkName string, driver string, labels map[string]string) (bool, error)
	RemoveNetwork(ctx context.Context, networkName string) error

	// Container operations
	StartContainer(ctx context.Context, containerID string, opts client.ContainerStartOptions) error
	CreateContainer(ctx context.Context, networkName string, opts CreateContainerOptions) (Container, error)
	StopContainer(ctx context.Context, containerID string, opts client.ContainerStopOptions) error
	RemoveContainer(ctx context.Context, containerID string) error

	// Exec operations
	Exec(ctx context.Context, containerID string, cmd []string) (*ExecResult, error)
	ExecStd(ctx context.Context, containerID string, cmd []string, opts ExecOptions) (*ExecResult, error)
	ExecStream(ctx context.Context, containerID string, cmd []string, stdout, stderr io.Writer, opts ExecOptions) (exitCode int, err error)

	// List operations
	ListContainersByLabels(ctx context.Context, labels ...Label) ([]Container, error)
}

var _ Docker = (*dockerClient)(nil)

// Labels for identifying pool containers
const (
	LabelManagedBy      = "dockerpool.managed-by"
	LabelManagedByValue = "dockerpool"
	LabelPoolName       = "dockerpool.pool-name"
)

// Container states
const (
	StateCreated    ContainerState = "created"
	StateRunning    ContainerState = "running"
	StatePaused     ContainerState = "paused"
	StateRestarting ContainerState = "restarting"
	StateRemoving   ContainerState = "removing"
	StateExited     ContainerState = "exited"
	StateDead       ContainerState = "dead"
)

type ContainerState string

// ExecResult contains the result of executing a command in a container
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// ExecOptions contains options for executing a command in a container
type ExecOptions struct {
	Stdin   io.Reader     // Input to pass to the command
	Timeout time.Duration // If > 0, command will be cancelled after timeout
	Limit   int64         // If > 0, the output will be limited to the given number of bytes
}

// Label represents a key-value pair for filtering containers
type Label struct {
	Key   string
	Value string
}

// dockerClient is the implementation of the Docker interface
type dockerClient struct {
	cli client.APIClient
}

// newDockerClient creates a new Docker client with default settings
func newDockerClient() (*dockerClient, error) {
	return newDockerClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// newDockerClientWithOpts creates a Docker client with custom options
func newDockerClientWithOpts(opts ...client.Opt) (*dockerClient, error) {
	cli, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return &dockerClient{cli: cli}, nil
}

// Close closes the connection to Docker
func (d *dockerClient) Close() error {
	return d.cli.Close()
}

// Ping checks connection to Docker daemon
func (d *dockerClient) Ping(ctx context.Context) error {
	_, err := d.cli.Ping(ctx, client.PingOptions{})
	if err != nil {
		return fmt.Errorf("failed to ping docker: %w", err)
	}
	return nil
}

// PullImage pulls an image from a registry and waits for completion
func (d *dockerClient) PullImage(ctx context.Context, image string) error {
	resp, err := d.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", image, err)
	}

	// Wait for pull to complete
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", image, err)
	}

	return nil
}

// EnsureNetwork creates a network if it does not exist.
// Returns true if the network was created, false if it already existed.
func (d *dockerClient) EnsureNetwork(ctx context.Context, networkName string, driver string, labels map[string]string) (bool, error) {
	_, err := d.cli.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{
		Driver: driver,
		Labels: MergeLabels(labels, map[string]string{
			LabelManagedBy: LabelManagedByValue,
		}),
	})
	if err != nil {
		return false, fmt.Errorf("failed to create network %s: %w", networkName, err)
	}

	return true, nil
}

// RemoveNetwork removes a network
func (d *dockerClient) RemoveNetwork(ctx context.Context, networkName string) error {
	_, err := d.cli.NetworkRemove(ctx, networkName, client.NetworkRemoveOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove network %s: %w", networkName, err)
	}
	return nil
}

// CreateContainerOptions contains options for creating a container
type CreateContainerOptions struct {
	Config     *container.Config     // Container configuration (image, cmd, env, labels, etc.)
	HostConfig *container.HostConfig // Host settings (volumes, network, resources, etc.)
}

func (d *dockerClient) StartContainer(ctx context.Context, containerID string, opts client.ContainerStartOptions) error {
	_, err := d.cli.ContainerStart(ctx, containerID, opts)
	if err != nil {
		return fmt.Errorf("failed to start container %s: %w", containerID, err)
	}
	return nil
}

// CreateContainer creates and starts a container.
// Returns the ID of the created container.
func (d *dockerClient) CreateContainer(ctx context.Context, networkName string, opts CreateContainerOptions) (Container, error) {
	if opts.Config == nil || opts.Config.Image == "" {
		return nil, ErrImageRequired
	}

	// Create container with network attached
	createResult, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     opts.Config,
		HostConfig: opts.HostConfig,
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkName: {},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	containerID := createResult.ID

	// Start container
	err = d.StartContainer(ctx, containerID, client.ContainerStartOptions{})
	if err != nil {
		startErr := fmt.Errorf("failed to start container: %w", err)
		if cleanupErr := d.RemoveContainer(ctx, containerID); cleanupErr != nil {
			return nil, fmt.Errorf("%w (cleanup also failed: %w)", startErr, cleanupErr)
		}
		return nil, startErr
	}
	container := NewContainerClient(ContainerOpts{
		ID:     containerID,
		Image:  opts.Config.Image,
		Labels: opts.Config.Labels,
		State:  StateRunning,
		Docker: d,
	})

	return container, nil
}

// StopContainer stops a running container
func (d *dockerClient) StopContainer(ctx context.Context, containerID string, opts client.ContainerStopOptions) error {
	_, err := d.cli.ContainerStop(ctx, containerID, opts)
	if err != nil {
		return fmt.Errorf("failed to stop container %s: %w", containerID, err)
	}
	return nil
}

// RemoveContainer stops and removes a container
func (d *dockerClient) RemoveContainer(ctx context.Context, containerID string) error {
	// First try to stop
	_, err := d.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{})
	if err != nil {
		return fmt.Errorf("failed to stop container %s: %w", containerID, err)
	}

	// Then force remove
	_, err = d.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	if err != nil {
		return fmt.Errorf("failed to remove container %s: %w", containerID, err)
	}

	return nil
}

// Exec executes a command in a container and returns the result
func (d *dockerClient) Exec(ctx context.Context, containerID string, cmd []string) (*ExecResult, error) {
	return d.ExecStd(ctx, containerID, cmd, ExecOptions{})
}

// ExecStd executes a command and returns output as strings in ExecResult.
// Use ExecStream if you need to stream output to io.Writer.
func (d *dockerClient) ExecStd(ctx context.Context, containerID string, cmd []string, opts ExecOptions) (*ExecResult, error) {
	var stdout, stderr bytes.Buffer
	exitCode, err := d.ExecStream(ctx, containerID, cmd, &stdout, &stderr, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to execute command: %w", err)
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   stderr.Bytes(),
		Stderr:   stderr.Bytes(),
	}, nil
}

// ExecStream executes a command and streams output to provided writers.
// Returns exit code directly. Use this for large outputs or real-time logging.
func (d *dockerClient) ExecStream(ctx context.Context, containerID string, cmd []string, stdout, stderr io.Writer, opts ExecOptions) (int, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	execCreateResult, err := d.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  opts.Stdin != nil,
	})
	if err != nil {
		return -1, fmt.Errorf("failed to create exec: %w", err)
	}

	execID := execCreateResult.ID

	attachResult, err := d.cli.ExecAttach(ctx, execID, client.ExecAttachOptions{
		TTY: false,
	})
	if err != nil {
		return -1, fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer attachResult.Close()

	errs := make(chan error, 1)
	if opts.Stdin != nil {
		go func() {
			_, err = io.Copy(attachResult.Conn, opts.Stdin)
			_ = attachResult.CloseWrite()
			errs <- err
			close(errs)
		}()
	}

	if err := decodeDockerStream(attachResult.Reader, opts.Limit, stdout, stderr); err != nil {
		return -1, fmt.Errorf("failed to read exec output %w: %w", err, <-errs)
	}

	inspectResult, err := d.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil {
		return -1, fmt.Errorf("failed to inspect exec %w: %w", err, <-errs)
	}

	return inspectResult.ExitCode, <-errs
}

// ListContainersByLabels returns a list of containers with the specified labels (AND)
func (d *dockerClient) ListContainersByLabels(ctx context.Context, labels ...Label) ([]Container, error) {
	filters := client.Filters{}

	for _, label := range labels {
		filters = filters.Add("label", fmt.Sprintf("%s=%s", label.Key, label.Value))
	}

	listResult, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true, // Including stopped
		Filters: filters,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	containers := make([]Container, 0, len(listResult.Items))
	for _, c := range listResult.Items {
		container := NewContainerClient(ContainerOpts{
			ID:     c.ID,
			Image:  c.Image,
			Labels: c.Labels,
			State:  ContainerState(c.State),
			Docker: d,
		})
		containers = append(containers, container)
	}

	return containers, nil
}

// PoolLabels creates base labels for a pool container
func PoolLabels(poolName string) map[string]string {
	return map[string]string{
		LabelManagedBy: LabelManagedByValue,
		LabelPoolName:  poolName,
	}
}

// MergeLabels merges multiple map[string]string into one.
// Later maps overwrite values from earlier ones.
func MergeLabels(labelMaps ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, m := range labelMaps {
		maps.Copy(result, m)
	}
	return result
}

const (
	headerSize     = 8
	initialBufSize = 32*1024 + headerSize // 32KB + header
)

// decodeDockerStream separates stdout and stderr from a multiplexed Docker stream.
// Docker uses format: [8]byte{STREAM_TYPE, 0, 0, 0, SIZE1, SIZE2, SIZE3, SIZE4} + data
func decodeDockerStream(reader io.Reader, limit int64, stdout, stderr io.Writer) error {
	buf := make([]byte, initialBufSize)
	if limit > 0 {
		reader = &limitedReader{r: reader, limit: limit}
	}
	for {
		// Read header (8 bytes)
		n, err := io.ReadFull(reader, buf[:headerSize])
		if err != nil {
			if errors.Is(err, io.EOF) && n == 0 {

				return nil
			}
			return err
		}

		streamType := buf[0]
		size := binary.BigEndian.Uint32(buf[4:headerSize])
		if size == 0 {
			continue
		}

		// Grow buffer if needed
		if int(size) > cap(buf) {
			buf = make([]byte, size)
		}

		// Read data into reusable buffer
		_, err = io.ReadFull(reader, buf[:size])
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		// Write to the corresponding writer
		switch streamType {
		case 1: // stdout
			if _, err := stdout.Write(buf[:size]); err != nil {
				return err
			}
		case 2: // stderr
			if _, err := stderr.Write(buf[:size]); err != nil {
				return err
			}
		}
	}
}

type limitedReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (l *limitedReader) Read(p []byte) (n int, err error) {
	n, err = l.r.Read(p)
	l.read += int64(n)

	if l.limit > 0 && l.read > l.limit {
		return n, ErrOutputLimitExceeded
	}
	return
}
