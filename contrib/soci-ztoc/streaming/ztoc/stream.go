//go:build socihelper

package ztoc

import (
	"io"

	"github.com/awslabs/soci-snapshotter/ztoc/compression"
)

// BuildZtocReader consumes gzip once, without seeking or retaining the layer.
// It validates the complete gzip stream even when the tar ends before padding.
func (b *Builder) BuildZtocReader(input io.Reader, span int64) (*Ztoc, error) {
	r, err := compression.NewGzipStream(input, span)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	metadata, size, err := metadataFromTarReader(r)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, err
	}
	return &Ztoc{
		Version: Version09, BuildToolIdentifier: b.buildToolIdentifier,
		TOC: TOC{FileMetadata: metadata}, UncompressedArchiveSize: size, CompressedArchiveSize: r.Size,
		CompressionInfo: CompressionInfo{
			MaxSpanID: compression.SpanID(len(r.Digests) - 1), SpanDigests: r.Digests,
			Checkpoints: r.Checkpoints, CompressionAlgorithm: compression.Gzip,
		},
	}, nil
}

// CompressZtoc reads tar metadata before compression, and receives checkpoints
// from the compressor. The compressed stream is never inflated or reread.
func (b *Builder) CompressZtoc(input io.Reader, output io.Writer, span int64, level int) (*Ztoc, error) {
	w, err := compression.NewGzipCompressor(output, span, level)
	if err != nil {
		return nil, err
	}
	r := io.TeeReader(input, w)
	metadata, size, err := metadataFromTarReader(r)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &Ztoc{
		Version: Version09, BuildToolIdentifier: b.buildToolIdentifier,
		TOC: TOC{FileMetadata: metadata}, UncompressedArchiveSize: size, CompressedArchiveSize: w.Size,
		CompressionInfo: CompressionInfo{
			MaxSpanID: compression.SpanID(len(w.Digests) - 1), SpanDigests: w.Digests,
			Checkpoints: w.Checkpoints, CompressionAlgorithm: compression.Gzip,
		},
	}, nil
}
