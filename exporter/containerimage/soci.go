package containerimage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/progress/logs"
	"github.com/moby/buildkit/util/push"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

const (
	sociArtifactType    = "application/vnd.amazon.soci.index.v2+json"
	sociIndexAnnotation = "com.amazon.soci.index-digest"
	sociSpanSize        = 4 << 20
	sociMinLayerSize    = 10 << 20
	sociCacheLabel      = "containerd.io/gc.ref.content.soci-0.16.1-4m-v1"
	sociBuildTool       = "BuildKit SOCI spike / soci v0.16.1"
)

// Bound native indexers across concurrent Bake targets, leaving CPU for compression.
var sociSlots = semaphore.NewWeighted(2)

type sociIndexer struct {
	store          content.Store
	helper         string
	layers         flightcontrol.CachedGroup[ocispecs.Descriptor]
	manifests      map[digest.Digest]ocispecs.Descriptor
	push           func(context.Context, ocispecs.Descriptor, content.Provider) error
	pushers        []*push.BlobPusher
	remote         map[digest.Digest]ocispecs.Descriptor
	remoteProvider content.Provider
}

func newSociIndexer(store content.Store) (*sociIndexer, error) {
	helper, err := exec.LookPath("buildkit-soci-ztoc")
	if err != nil {
		return nil, errors.Wrap(err, "soci=true requires the bundled buildkit-soci-ztoc helper")
	}
	return &sociIndexer{store: store, helper: helper, manifests: make(map[digest.Digest]ocispecs.Descriptor)}, nil
}

func (s *sociIndexer) indexLayer(ctx context.Context, layer ocispecs.Descriptor, provider content.Provider) error {
	eg, ctx := errgroup.WithContext(ctx)
	if s.push != nil {
		eg.Go(func() error { return s.push(ctx, layer, provider) })
	}
	if layer.Size >= sociMinLayerSize {
		eg.Go(func() error {
			ztoc, err := s.layer(ctx, layer, provider)
			if err != nil {
				return err
			}
			if s.push != nil {
				return s.push(ctx, ztoc, s.store)
			}
			return nil
		})
	}
	return eg.Wait()
}

func (s *sociIndexer) layer(ctx context.Context, layer ocispecs.Descriptor, provider content.Provider) (ocispecs.Descriptor, error) {
	return s.layers.Do(ctx, layer.Digest.String(), func(ctx context.Context) (out ocispecs.Descriptor, retErr error) {
		if layer.MediaType != ocispecs.MediaTypeImageLayerGzip && layer.MediaType != images.MediaTypeDockerSchema2LayerGzip {
			return out, errors.Errorf("SOCI requires gzip layer %s, got %s", layer.Digest, layer.MediaType)
		}
		if info, err := s.store.Info(ctx, layer.Digest); err == nil {
			if cached := info.Labels[sociCacheLabel]; cached != "" {
				if ztocInfo, err := s.store.Info(ctx, digest.Digest(cached)); err == nil {
					progress.OneOff(ctx, "reusing SOCI ztoc "+layer.Digest.String())(nil)
					return sociLayerDescriptor(layer, ztocInfo.Digest, ztocInfo.Size), nil
				} else if !errdefs.IsNotFound(err) {
					return out, err
				}
			}
		} else if !errdefs.IsNotFound(err) {
			return out, err
		}
		if cached, ok := s.remote[layer.Digest]; ok && cached.Annotations["com.amazon.soci.image-layer-mediaType"] == layer.MediaType {
			done := progress.OneOff(ctx, "reusing remote SOCI ztoc "+layer.Digest.String())
			defer func() { done(retErr) }()
			if err := contentutil.Copy(ctx, s.store, s.remoteProvider, cached, "", logs.LoggerFromContext(ctx)); err != nil {
				return out, err
			}
			out = sociLayerDescriptor(layer, cached.Digest, cached.Size)
			// A lazy imported layer may not be local. The output manifest and solve
			// lease retain its zTOC without forcing the layer itself to be fetched.
			_, err := s.store.Update(ctx, content.Info{Digest: layer.Digest, Labels: map[string]string{sociCacheLabel: out.Digest.String()}}, "labels."+sociCacheLabel)
			if errdefs.IsNotFound(err) {
				err = nil
			}
			return out, err
		}
		if err := sociSlots.Acquire(ctx, 1); err != nil {
			return out, err
		}
		defer sociSlots.Release(1)
		done := progress.OneOff(ctx, "indexing SOCI layer "+layer.Digest.String())
		defer func() { done(retErr) }()
		dir, err := os.MkdirTemp("", "buildkit-soci-")
		if err != nil {
			return out, err
		}
		defer os.RemoveAll(dir)
		ra, err := provider.ReaderAt(ctx, layer)
		if err != nil {
			return out, err
		}
		defer ra.Close()
		// containerd's local content reader exposes the file through a LimitedReader.
		// Inherit that descriptor instead of copying the whole layer for the helper.
		reader, ok := content.NewReader(ra).(*io.LimitedReader)
		if !ok {
			return out, errors.New("SOCI indexing requires a local file-backed content store")
		}
		input, ok := reader.R.(*os.File)
		if !ok {
			return out, errors.New("SOCI indexing requires a local file-backed content store")
		}
		if ra.Size() != layer.Size {
			return out, errors.Errorf("SOCI layer size mismatch: got %d, want %d", ra.Size(), layer.Size)
		}
		output, err := os.Create(filepath.Join(dir, "ztoc"))
		if err != nil {
			return out, err
		}
		defer output.Close()
		cmd := exec.CommandContext(ctx, s.helper, "/proc/self/fd/3")
		cmd.ExtraFiles = []*os.File{input}
		cmd.Stdout = output
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return out, errors.Wrapf(err, "SOCI indexer: %s", stderr.String())
		}
		out, err = s.writeZtoc(ctx, layer, output)
		if err != nil {
			return out, err
		}
		_, err = s.store.Update(ctx, content.Info{Digest: layer.Digest, Labels: map[string]string{sociCacheLabel: out.Digest.String()}}, "labels."+sociCacheLabel)
		return out, err
	})
}

func sociLayerDescriptor(layer ocispecs.Descriptor, dgst digest.Digest, size int64) ocispecs.Descriptor {
	return ocispecs.Descriptor{
		MediaType: "application/octet-stream", Digest: dgst, Size: size,
		Annotations: map[string]string{
			"com.amazon.soci.image-layer-digest":    layer.Digest.String(),
			"com.amazon.soci.image-layer-mediaType": layer.MediaType,
			"com.amazon.soci.span-size":             strconv.Itoa(sociSpanSize),
		},
	}
}

func (s *sociIndexer) annotate(ctx context.Context, manifest *ocispecs.Manifest, provider content.Provider) (*ocispecs.Descriptor, error) {
	var ztocs []ocispecs.Descriptor
	for _, layer := range manifest.Layers {
		if layer.Size < sociMinLayerSize {
			continue
		}
		ztoc, err := s.layer(ctx, layer, provider)
		if err != nil {
			return nil, err
		}
		ztocs = append(ztocs, ztoc)
	}
	if len(ztocs) == 0 {
		return nil, nil
	}
	config := ocispecs.Descriptor{MediaType: sociArtifactType, Digest: digest.FromString("{}"), Size: 2}
	if err := content.WriteBlob(ctx, s.store, config.Digest.String(), bytes.NewReader([]byte("{}")), config); err != nil {
		return nil, err
	}
	index := ocispecs.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispecs.MediaTypeImageManifest,
		Config: config, Layers: ztocs,
		Annotations: map[string]string{"com.amazon.soci.build-tool-identifier": sociBuildTool},
	}
	desc, err := s.writeJSON(ctx, index, index.MediaType, append([]ocispecs.Descriptor{config}, ztocs...))
	if err != nil {
		return nil, err
	}
	desc.ArtifactType = sociArtifactType
	if manifest.Annotations == nil {
		manifest.Annotations = make(map[string]string)
	}
	manifest.Annotations[sociIndexAnnotation] = desc.Digest.String()
	return desc, nil
}

func (s *sociIndexer) wrap(ctx context.Context, root *ocispecs.Descriptor) (*ocispecs.Descriptor, error) {
	if len(s.manifests) == 0 {
		return root, nil
	}
	idx := ocispecs.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispecs.MediaTypeImageIndex}
	if images.IsIndexType(root.MediaType) {
		data, err := content.ReadBlob(ctx, s.store, *root)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &idx); err != nil {
			return nil, err
		}
	} else {
		image := *root
		if image.Platform == nil {
			ps, err := images.Platforms(ctx, s.store, image)
			if err != nil || len(ps) != 1 {
				return nil, errors.Errorf("SOCI image platform: %v (%d platforms)", err, len(ps))
			}
			image.Platform = &ps[0]
		}
		idx.Manifests = []ocispecs.Descriptor{image}
	}
	for _, image := range idx.Manifests {
		if artifact, ok := s.manifests[image.Digest]; ok {
			artifact.Platform = image.Platform
			artifact.Annotations = map[string]string{"com.amazon.soci.image-manifest-digest": image.Digest.String()}
			idx.Manifests = append(idx.Manifests, artifact)
		}
	}
	desc, err := s.writeJSON(ctx, idx, idx.MediaType, idx.Manifests)
	if err == nil {
		desc.Annotations = root.Annotations
	}
	return desc, err
}

func (s *sociIndexer) writeJSON(ctx context.Context, value any, mediaType string, children []ocispecs.Descriptor) (*ocispecs.Descriptor, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	desc := ocispecs.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	labels := make(map[string]string, len(children))
	for i, child := range children {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.%d", i)] = child.Digest.String()
	}
	return &desc, content.WriteBlob(ctx, s.store, desc.Digest.String(), bytes.NewReader(data), desc, content.WithLabels(labels))
}
