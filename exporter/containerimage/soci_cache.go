package containerimage

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/progress/logs"
	"github.com/moby/buildkit/util/resolver"
	resolverconfig "github.com/moby/buildkit/util/resolver/config"
	"github.com/moby/buildkit/util/resolver/limited"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Read only the index manifests here. Fetch a zTOC only if the output uses its
// exact compressed layer digest; never pull image layers to populate this cache.
func (e *imageExporterInstance) importSOCICache(ctx context.Context, s *sociIndexer, ref, sid string) (retErr error) {
	done := progress.OneOff(ctx, "importing SOCI cache "+ref)
	defer func() { done(retErr) }()
	parsed, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return err
	}
	ref = reference.TagNameOnly(parsed).String()
	hosts := e.opt.RegistryHosts
	if e.insecure {
		yes := true
		hosts = resolver.NewRegistryConfig(map[string]resolverconfig.RegistryConfig{
			reference.Domain(parsed): {Insecure: &yes, PlainHTTP: &yes},
		})
	}
	r := resolver.DefaultPool.GetResolver(hosts, ref, resolver.ScopeType{Insecure: e.insecure}, e.opt.SessionManager, session.NewGroup(sid))
	xref, root, err := r.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	fetcher, err := r.Fetcher(ctx, xref)
	if err != nil {
		return err
	}
	s.remoteProvider = contentutil.FromFetcher(limited.Default.WrapFetcher(fetcher, ref))
	s.remote = make(map[digest.Digest]ocispecs.Descriptor)
	read := func(desc ocispecs.Descriptor, value any) error {
		if err := contentutil.Copy(ctx, s.store, s.remoteProvider, desc, "", logs.LoggerFromContext(ctx)); err != nil {
			return err
		}
		data, err := content.ReadBlob(ctx, s.store, desc)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, value)
	}
	// Non-SOCI images are legitimate empty cache sources.
	if !images.IsIndexType(root.MediaType) {
		return nil
	}
	var index ocispecs.Index
	if err := read(root, &index); err != nil {
		return err
	}
	for _, desc := range index.Manifests {
		if desc.ArtifactType != sociArtifactType {
			continue
		}
		var manifest ocispecs.Manifest
		if err := read(desc, &manifest); err != nil {
			return err
		}
		if manifest.Config.MediaType != sociArtifactType || manifest.Annotations["com.amazon.soci.build-tool-identifier"] != sociBuildTool {
			continue
		}
		for _, ztoc := range manifest.Layers {
			if ztoc.MediaType != "application/octet-stream" || ztoc.Annotations["com.amazon.soci.span-size"] != strconv.Itoa(sociSpanSize) {
				continue
			}
			layer := digest.Digest(ztoc.Annotations["com.amazon.soci.image-layer-digest"])
			if err := layer.Validate(); err != nil {
				return errors.Wrap(err, "invalid SOCI cache layer digest")
			}
			s.remote[layer] = ztoc
		}
	}
	return nil
}
