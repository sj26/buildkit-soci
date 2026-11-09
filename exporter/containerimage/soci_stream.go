package containerimage

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/util/iohelper"
	"github.com/moby/buildkit/util/progress"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// newStream compresses tar and creates checkpoints in the same helper. The pipe
// applies backpressure; the two-slot budget also covers existing-blob indexing.
func (s *sociIndexer) newStream(ctx context.Context, dest io.Writer, level int) (io.WriteCloser, compression.Finalizer, error) {
	if err := sociSlots.Acquire(ctx, 1); err != nil {
		return nil, nil, err
	}
	output, err := os.CreateTemp("", "buildkit-soci-ztoc-")
	if err != nil {
		sociSlots.Release(1)
		return nil, nil, err
	}
	// Only the open descriptor is needed, including if the solve is canceled.
	os.Remove(output.Name())
	cmd := exec.CommandContext(ctx, s.helper, "--compress", strconv.Itoa(level))
	writer, err := cmd.StdinPipe()
	if err != nil {
		output.Close()
		sociSlots.Release(1)
		return nil, nil, err
	}
	hash := digest.Canonical.Digester()
	var size iohelper.Counter
	cmd.Stdout = io.MultiWriter(dest, hash.Hash(), &size)
	cmd.ExtraFiles = []*os.File{output}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		writer.Close()
		output.Close()
		sociSlots.Release(1)
		return nil, nil, err
	}
	done := progress.OneOff(ctx, "streaming SOCI index")
	var layer, ztoc ocispecs.Descriptor
	closeStream := sync.OnceValue(func() (retErr error) {
		defer sociSlots.Release(1)
		defer output.Close()
		defer func() { done(retErr) }()
		writer.Close()
		if err := cmd.Wait(); err != nil {
			return errors.Wrapf(err, "SOCI indexer: %s", stderr.String())
		}
		layer = ocispecs.Descriptor{Digest: hash.Digest(), Size: size.Size(), MediaType: ocispecs.MediaTypeImageLayerGzip}
		if layer.Size < sociMinLayerSize {
			return nil
		}
		var err error
		ztoc, err = s.writeZtoc(ctx, layer, output)
		return err
	})
	return &sociStreamWriter{Writer: writer, close: closeStream},
		func(ctx context.Context, store content.Store) (map[string]string, error) {
			if err := closeStream(); err != nil {
				return nil, err
			}
			if layer.Size < sociMinLayerSize {
				return nil, nil
			}
			_, err := store.Update(ctx, content.Info{Digest: layer.Digest, Labels: map[string]string{sociCacheLabel: ztoc.Digest.String()}}, "labels."+sociCacheLabel)
			return nil, err
		}, nil
}

type sociStreamWriter struct {
	io.Writer
	close func() error
}

func (w *sociStreamWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil {
		if indexErr := w.close(); indexErr != nil {
			return n, indexErr
		}
	}
	return n, err
}

func (w *sociStreamWriter) Close() error { return w.close() }

func (s *sociIndexer) writeZtoc(ctx context.Context, layer ocispecs.Descriptor, output *os.File) (ocispecs.Descriptor, error) {
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return ocispecs.Descriptor{}, err
	}
	dgst, err := digest.FromReader(output)
	if err != nil {
		return ocispecs.Descriptor{}, err
	}
	size, err := output.Seek(0, io.SeekCurrent)
	if err != nil {
		return ocispecs.Descriptor{}, err
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return ocispecs.Descriptor{}, err
	}
	desc := sociLayerDescriptor(layer, dgst, size)
	return desc, content.WriteBlob(ctx, s.store, dgst.String(), output, desc)
}
