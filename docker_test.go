package dockerpool

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

// createDockerFrame creates a Docker multiplexed stream frame
// streamType: 1 = stdout, 2 = stderr
func createDockerFrame(streamType byte, data []byte) []byte {
	frame := make([]byte, 8+len(data))
	frame[0] = streamType
	// bytes 1-3 are reserved (zeros)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(data)))
	copy(frame[8:], data)
	return frame
}

func TestDecodeDockerStream_StdoutOnly(t *testing.T) {
	data := []byte("Hello from stdout!")
	stream := createDockerFrame(1, data)

	reader := bytes.NewReader(stream)
	var stdout, stderr bytes.Buffer

	err := decodeDockerStream(reader, 0, &stdout, &stderr)
	assert.NoError(t, err)
	assert.Equal(t, "Hello from stdout!", stdout.String())
	assert.Equal(t, "", stderr.String())
}

func TestDecodeDockerStream_StderrOnly(t *testing.T) {
	data := []byte("Hello from stderr!")
	stream := createDockerFrame(2, data)

	reader := bytes.NewReader(stream)
	var stdout, stderr bytes.Buffer

	err := decodeDockerStream(reader, 0, &stdout, &stderr)
	assert.NoError(t, err)
	assert.Equal(t, "", stdout.String())
	assert.Equal(t, "Hello from stderr!", stderr.String())
}
func TestDockerStream_OutputLimit(t *testing.T) {
	data := []byte("Hello from stdout!")
	stream := createDockerFrame(1, data)

	reader := bytes.NewReader(stream)
	var stdout, stderr bytes.Buffer

	err := decodeDockerStream(reader, 10, &stdout, &stderr)
	assert.ErrorIs(t, err, ErrOutputLimitExceeded)
}

func BenchmarkDecodeDockerStream(b *testing.B) {
	var stream []byte
	const kbi = 1024
	data := make([]byte, kbi)
	for i := 0; i < kbi; i++ {
		stream = append(stream, createDockerFrame(1, data)...)
	}
	reader := bytes.NewReader(stream)
	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader.Reset(stream)
		decodeDockerStream(reader, 0, io.Discard, io.Discard)
	}
}
