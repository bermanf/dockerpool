package dockerpool

import "errors"

var (
	// ErrImageRequired is returned when Docker image is not specified
	ErrImageRequired = errors.New("image is required")

	// ErrOutputLimitExceeded is returned when exec output exceeds the limit
	ErrOutputLimitExceeded = errors.New("output limit exceeded")

	// ErrPoolNameRequired is returned when pool name is empty
	ErrPoolNameRequired = errors.New("pool name is required")

	// ErrNetworkNameRequired is returned when network name is empty
	ErrNetworkNameRequired = errors.New("network name is required")

	// ErrPoolShutdown is returned when operations are attempted on a shut down pool
	ErrPoolShutdown = errors.New("pool is shut down")
)
