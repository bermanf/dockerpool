package dockerpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDockerClient creates a real Docker client, skipping the test if Docker is unavailable.
func newTestDockerClient(t *testing.T) *dockerClient {
	t.Helper()

	cli, err := newDockerClient()
	require.NoError(t, err, "create docker client")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Ping(ctx); err != nil {
		cli.Close()
		t.Skipf("Docker daemon not available: %v", err)
	}

	t.Cleanup(func() { cli.Close() })
	return cli
}

// uniqueName returns a unique name suitable for Docker resources.
func uniqueName(t *testing.T) string {
	t.Helper()
	safe := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("dp-%s-%x", safe, time.Now().UnixNano())
}

// setupTestNetwork creates a unique bridge network and registers cleanup.
func setupTestNetwork(t *testing.T, cli *dockerClient) string {
	t.Helper()
	name := uniqueName(t)
	_, err := cli.EnsureNetwork(context.Background(), name, "bridge", nil)
	require.NoError(t, err, "create test network")
	t.Cleanup(func() { cli.RemoveNetwork(context.Background(), name) })
	return name
}

// createTestContainer creates a container running "sleep infinity" and registers cleanup.
func createTestContainer(t *testing.T, cli *dockerClient, networkName string) Container {
	t.Helper()
	c, err := cli.CreateContainer(context.Background(), networkName, CreateContainerOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"sleep", "infinity"},
		},
	})
	require.NoError(t, err, "create test container")
	t.Cleanup(func() { cli.RemoveContainer(context.Background(), c.ID()) })
	return c
}

func TestDockerClient_Ping(t *testing.T) {
	cli := newTestDockerClient(t)
	err := cli.Ping(context.Background())
	assert.NoError(t, err)
}

func TestDockerClient_Ping_CancelledContext(t *testing.T) {
	cli := newTestDockerClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling
	err := cli.Ping(ctx)
	assert.Error(t, err)
}

func TestDockerClient_EnsureNetwork(t *testing.T) {
	cli := newTestDockerClient(t)
	name := uniqueName(t)

	created, err := cli.EnsureNetwork(context.Background(), name, "bridge", map[string]string{"test": "true"})
	require.NoError(t, err)
	assert.True(t, created)

	t.Cleanup(func() { cli.RemoveNetwork(context.Background(), name) })
}

func TestDockerClient_RemoveNetwork(t *testing.T) {
	cli := newTestDockerClient(t)
	name := uniqueName(t)
	_, err := cli.EnsureNetwork(context.Background(), name, "bridge", nil)
	require.NoError(t, err)

	err = cli.RemoveNetwork(context.Background(), name)
	assert.NoError(t, err)
}

func TestDockerClient_CreateContainer(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)

	c, err := cli.CreateContainer(context.Background(), networkName, CreateContainerOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"sleep", "infinity"},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { cli.RemoveContainer(context.Background(), c.ID()) })

	assert.NotEmpty(t, c.ID())
	assert.Equal(t, "alpine:latest", c.Image())
	assert.Equal(t, StateRunning, c.State())
}

func TestDockerClient_CreateContainer_WithLabels(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)

	labels := map[string]string{"app": "test", "env": "ci"}
	c, err := cli.CreateContainer(context.Background(), networkName, CreateContainerOptions{
		Config: &container.Config{
			Image:  "alpine:latest",
			Cmd:    []string{"sleep", "infinity"},
			Labels: labels,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { cli.RemoveContainer(context.Background(), c.ID()) })

	assert.Equal(t, "test", c.Labels()["app"])
	assert.Equal(t, "ci", c.Labels()["env"])
}

func TestDockerClient_CreateContainer_MissingImage(t *testing.T) {
	cli := newTestDockerClient(t)

	_, err := cli.CreateContainer(context.Background(), "any-network", CreateContainerOptions{
		Config: &container.Config{},
	})
	assert.ErrorIs(t, err, ErrImageRequired)
}

func TestDockerClient_CreateContainer_NilConfig(t *testing.T) {
	cli := newTestDockerClient(t)

	_, err := cli.CreateContainer(context.Background(), "any-network", CreateContainerOptions{})
	assert.ErrorIs(t, err, ErrImageRequired)
}

func TestDockerClient_StopAndStartContainer(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)
	ctx := context.Background()

	err := cli.StopContainer(ctx, c.ID(), client.ContainerStopOptions{})
	require.NoError(t, err)

	err = cli.StartContainer(ctx, c.ID(), client.ContainerStartOptions{})
	assert.NoError(t, err)
}

func TestDockerClient_RemoveContainer(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)

	c, err := cli.CreateContainer(context.Background(), networkName, CreateContainerOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"sleep", "infinity"},
		},
	})
	require.NoError(t, err)
	// Fallback cleanup in case the assertion below fails.
	t.Cleanup(func() { cli.RemoveContainer(context.Background(), c.ID()) })

	err = cli.RemoveContainer(context.Background(), c.ID())
	assert.NoError(t, err)
}

func TestDockerClient_Exec_Stdout(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	result, err := cli.Exec(context.Background(), c.ID(), []string{"echo", "hello"})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "hello\n", string(result.Stdout))
	assert.Empty(t, result.Stderr)
}

func TestDockerClient_Exec_NonZeroExitCode(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	result, err := cli.Exec(context.Background(), c.ID(), []string{"sh", "-c", "exit 42"})
	require.NoError(t, err) // non-zero exit is not an error from Exec's perspective
	assert.Equal(t, 42, result.ExitCode)
}

func TestDockerClient_ExecStd_Stderr(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	result, err := cli.ExecStd(context.Background(), c.ID(),
		[]string{"sh", "-c", "echo err_output >&2"},
		ExecOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "err_output\n", string(result.Stderr))
	assert.Empty(t, result.Stdout)
}

func TestDockerClient_ExecStd_WithStdin(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	result, err := cli.ExecStd(context.Background(), c.ID(),
		[]string{"cat"},
		ExecOptions{Stdin: strings.NewReader("hello from stdin\n")},
	)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "hello from stdin\n", string(result.Stdout))
}

func TestDockerClient_ExecStd_Timeout(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	_, err := cli.ExecStd(context.Background(), c.ID(),
		[]string{"sleep", "10"},
		ExecOptions{Timeout: 100 * time.Millisecond},
	)
	assert.Error(t, err)
}

func TestDockerClient_ExecStream(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	var stdout, stderr bytes.Buffer
	exitCode, err := cli.ExecStream(
		context.Background(), c.ID(),
		[]string{"sh", "-c", "echo stdout_line; echo stderr_line >&2"},
		&stdout, &stderr,
		ExecOptions{},
	)
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "stdout_line\n", stdout.String())
	assert.Equal(t, "stderr_line\n", stderr.String())
}

type errReader struct{ err error }

func (e *errReader) Read(_ []byte) (int, error) { return 0, e.err }

func TestDockerClient_ExecStreamWithStdin(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	input := strings.NewReader("hello from stdin\n")

	var stdout, stderr bytes.Buffer
	exitCode, err := cli.ExecStream(
		context.Background(), c.ID(),
		[]string{"sh", "-c", "cat"},
		&stdout, &stderr,
		ExecOptions{Stdin: input},
	)
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)
	assert.Equal(t, "hello from stdin\n", stdout.String())
	assert.Empty(t, stderr.String())
}

func TestDockerClient_ExecStreamStdinReadError(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	stdinErr := errors.New("stdin read error")

	var stdout, stderr bytes.Buffer
	_, err := cli.ExecStream(
		context.Background(), c.ID(),
		[]string{"cat"},
		&stdout, &stderr,
		ExecOptions{Stdin: &errReader{err: stdinErr}},
	)
	require.ErrorIs(t, err, stdinErr)
}
func TestDockerClient_ExecStreamWithLimitStdin(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	c := createTestContainer(t, cli, networkName)

	input := strings.NewReader("hello from stdin\n")

	var stdout, stderr bytes.Buffer
	_, err := cli.ExecStream(
		context.Background(), c.ID(),
		[]string{"sh", "-c", "cat"},
		&stdout, &stderr,
		ExecOptions{Stdin: input, Limit: 1},
	)
	require.ErrorContains(t, err, "failed to read exec output: output limit exceeded")
}

func TestDockerClient_ListContainersByLabels(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	ctx := context.Background()

	poolName := uniqueName(t)
	c, err := cli.CreateContainer(ctx, networkName, CreateContainerOptions{
		Config: &container.Config{
			Image:  "alpine:latest",
			Cmd:    []string{"sleep", "infinity"},
			Labels: PoolLabels(poolName),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { cli.RemoveContainer(context.Background(), c.ID()) })

	containers, err := cli.ListContainersByLabels(ctx,
		Label{Key: LabelManagedBy, Value: LabelManagedByValue},
		Label{Key: LabelPoolName, Value: poolName},
	)
	require.NoError(t, err)
	require.Len(t, containers, 1)
	assert.Equal(t, c.ID(), containers[0].ID())
	assert.Equal(t, "alpine:latest", containers[0].Image())
}

func TestDockerClient_ListContainersByLabels_NoMatch(t *testing.T) {
	cli := newTestDockerClient(t)

	containers, err := cli.ListContainersByLabels(context.Background(),
		Label{Key: LabelPoolName, Value: "nonexistent-pool-xyz-12345"},
	)
	require.NoError(t, err)
	assert.Empty(t, containers)
}

func TestDockerClient_ListContainersByLabels_FiltersByPool(t *testing.T) {
	cli := newTestDockerClient(t)
	networkName := setupTestNetwork(t, cli)
	ctx := context.Background()

	poolA := uniqueName(t) + "-a"
	poolB := uniqueName(t) + "-b"

	cA, err := cli.CreateContainer(ctx, networkName, CreateContainerOptions{
		Config: &container.Config{
			Image:  "alpine:latest",
			Cmd:    []string{"sleep", "infinity"},
			Labels: PoolLabels(poolA),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { cli.RemoveContainer(context.Background(), cA.ID()) })

	cB, err := cli.CreateContainer(ctx, networkName, CreateContainerOptions{
		Config: &container.Config{
			Image:  "alpine:latest",
			Cmd:    []string{"sleep", "infinity"},
			Labels: PoolLabels(poolB),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { cli.RemoveContainer(context.Background(), cB.ID()) })

	containersA, err := cli.ListContainersByLabels(ctx, Label{Key: LabelPoolName, Value: poolA})
	require.NoError(t, err)
	require.Len(t, containersA, 1)
	assert.Equal(t, cA.ID(), containersA[0].ID())

	containersB, err := cli.ListContainersByLabels(ctx, Label{Key: LabelPoolName, Value: poolB})
	require.NoError(t, err)
	require.Len(t, containersB, 1)
	assert.Equal(t, cB.ID(), containersB[0].ID())
}

func TestDockerClient_PullImage(t *testing.T) {
	cli := newTestDockerClient(t)
	// alpine is small and typically already cached locally.
	err := cli.PullImage(context.Background(), "alpine:latest")
	assert.NoError(t, err)
}
