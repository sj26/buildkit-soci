package llbsolver

import (
	"context"

	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/worker"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type earlyExportKey struct{}

func (b *llbBridge) buildWithEarlyExport(ctx context.Context, edge solver.Edge) (solver.CachedResultWithProvenance, error) {
	prepare, _ := ctx.Value(earlyExportKey{}).([]exporter.PrepareFunc)
	if len(prepare) == 0 {
		return b.builder.Build(ctx, edge)
	}
	// Do not pass the final-image callbacks into nested frontend solves.
	ctx = context.WithValue(ctx, earlyExportKey{}, nil)
	var ancestors []solver.Edge
	seen := make(map[solver.Edge]struct{})
	var visit func(solver.Edge)
	visit = func(edge solver.Edge) {
		for _, parent := range imageParents(edge) {
			if _, ok := seen[parent]; ok {
				continue
			}
			seen[parent] = struct{}{}
			visit(parent)
			ancestors = append(ancestors, parent)
		}
	}
	visit(edge)
	if len(ancestors) == 0 {
		return b.builder.Build(ctx, edge)
	}
	eg, runCtx := errgroup.WithContext(ctx)
	var result solver.CachedResultWithProvenance
	eg.Go(func() error {
		var err error
		result, err = b.builder.Build(runCtx, edge)
		return err
	})
	// One preparation loop per output: await existing solver edges, oldest first,
	// without serializing the main solve or spawning a task for every vertex.
	eg.Go(func() error {
		return inBuilderContext(runCtx, b.builder, "preparing image layers during solve", "early-"+edge.Vertex.Digest().String(), func(ctx context.Context, _ solver.JobContext) error {
			for _, ancestor := range ancestors {
				res, err := b.builder.Build(ctx, ancestor)
				if err != nil {
					return err
				}
				err = func() error {
					defer res.Release(context.WithoutCancel(ctx))
					ref, ok := res.Sys().(*worker.WorkerRef)
					if !ok {
						return errors.Errorf("invalid early export reference: %T", res.Sys())
					}
					if ref.ImmutableRef != nil {
						for _, f := range prepare {
							if err := f(ctx, ref.ImmutableRef); err != nil {
								return err
							}
						}
					}
					return nil
				}()
				if err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err := eg.Wait(); err != nil {
		if result != nil {
			result.Release(context.WithoutCancel(ctx))
		}
		return nil, err
	}
	return result, nil
}

// Follow only the retained root filesystem, never COPY sources or RUN mounts.
// Merge inputs (COPY --link) are retained; arbitrary diff inputs are not.
func imageParents(edge solver.Edge) []solver.Edge {
	op, ok := edge.Vertex.Sys().(*pb.Op)
	if !ok || op.Platform != nil && op.Platform.OS != "linux" {
		return nil
	}
	inputs := edge.Vertex.Inputs()
	input := int64(-1)
	switch op := op.Op.(type) {
	case *pb.Op_Merge:
		var parents []solver.Edge
		for _, input := range op.Merge.Inputs {
			if input.Input < 0 || input.Input >= int64(len(inputs)) {
				return nil
			}
			parents = append(parents, inputs[input.Input])
		}
		return parents
	case *pb.Op_Exec:
		for _, mount := range op.Exec.Mounts {
			if mount.Dest == "/" && mount.Output == int64(edge.Index) {
				input = mount.Input
				break
			}
		}
	case *pb.Op_File:
		for _, action := range op.File.Actions {
			if action.Output == int64(edge.Index) {
				input = action.Input
				break
			}
		}
		// File action inputs can refer to earlier actions within this vertex.
		for steps := 0; input >= int64(len(inputs)) && steps < len(op.File.Actions); steps++ {
			index := input - int64(len(inputs))
			if index >= int64(len(op.File.Actions)) {
				return nil
			}
			input = op.File.Actions[index].Input
		}
	default:
		return nil
	}
	if input < 0 || input >= int64(len(inputs)) {
		return nil
	}
	return []solver.Edge{inputs[input]}
}
