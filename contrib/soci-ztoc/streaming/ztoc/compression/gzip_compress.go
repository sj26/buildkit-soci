//go:build socihelper

package compression

import (
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/opencontainers/go-digest"
)

// GzipCompressor records byte-aligned deflate restart points while compressing.
// Flush retains the dictionary; checkpoints contain its last 32 KiB, not a
// second inflation of the emitted bytes.
type GzipCompressor struct {
	writer      *gzip.Writer
	output      io.Writer
	span, plain int64
	window      [32768]byte
	used        int
	hash        digest.Digester
	Digests     []digest.Digest
	Checkpoints []byte
	Size        Offset
}

func NewGzipCompressor(output io.Writer, span int64, level int) (*GzipCompressor, error) {
	if span <= 0 {
		return nil, fmt.Errorf("invalid span size %d", span)
	}
	c := &GzipCompressor{output: output, span: span, Checkpoints: make([]byte, 12)}
	w, err := gzip.NewWriterLevel(compressedOutput{c}, level)
	if err != nil {
		return nil, err
	}
	c.writer = w
	// Emit the gzip header before the first raw-deflate checkpoint.
	if _, err := w.Write(nil); err != nil {
		return nil, err
	}
	c.checkpoint()
	return c, nil
}

type compressedOutput struct{ c *GzipCompressor }

func (w compressedOutput) Write(p []byte) (int, error) {
	n, err := w.c.output.Write(p)
	w.c.Size += Offset(n)
	if w.c.hash != nil {
		w.c.hash.Hash().Write(p[:n])
	}
	return n, err
}

func (c *GzipCompressor) checkpoint() {
	c.Checkpoints = binary.LittleEndian.AppendUint64(c.Checkpoints, uint64(c.Size))
	c.Checkpoints = binary.LittleEndian.AppendUint64(c.Checkpoints, uint64(c.plain))
	c.Checkpoints = append(c.Checkpoints, 0) // Flush ends on a byte boundary.
	c.Checkpoints = append(c.Checkpoints, c.window[c.used:]...)
	c.Checkpoints = append(c.Checkpoints, c.window[:c.used]...)
	c.hash = digest.Canonical.Digester()
}

func (c *GzipCompressor) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if c.plain > 0 && c.plain%c.span == 0 {
			if err := c.writer.Flush(); err != nil {
				return written, err
			}
			c.Digests = append(c.Digests, c.hash.Digest())
			c.checkpoint()
		}
		n, err := c.writer.Write(p[:min(int64(len(p)), c.span-c.plain%c.span)])
		window := p[:n]
		if len(window) >= len(c.window) {
			window = window[len(window)-len(c.window):]
			c.used = 0
		}
		for len(window) > 0 {
			k := copy(c.window[c.used:], window)
			c.used = (c.used + k) % len(c.window)
			window = window[k:]
		}
		written += n
		c.plain += int64(n)
		p = p[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (c *GzipCompressor) Close() error {
	if err := c.writer.Close(); err != nil {
		return err
	}
	c.Digests = append(c.Digests, c.hash.Digest())
	binary.LittleEndian.PutUint32(c.Checkpoints, uint32(len(c.Digests)))
	binary.LittleEndian.PutUint64(c.Checkpoints[4:], uint64(c.span))
	return nil
}
