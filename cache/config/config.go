package config

import (
	"context"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/moby/buildkit/util/compression"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

type RefConfig struct {
	Compression            compression.Config
	PreferNonDistributable bool
	// OnBlobReady runs after each layer's compression, concurrently with other layers.
	OnBlobReady func(context.Context, ocispecs.Descriptor, content.Provider) error
}
