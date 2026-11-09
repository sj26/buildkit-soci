//go:build socihelper

package compression

/*
#include <stdlib.h>
#include <stdint.h>
#include <zlib.h>

// A pull-based Z_BLOCK inflater. Each call ends at a block/input/window boundary;
// Go consumes its output and hashes exactly its consumed input before continuing.
typedef struct {
    z_stream z;
    unsigned char window[32768];
    uint64_t in, out, last;
    unsigned used, produced, consumed, checkpoint, bits;
} soci_stream;

static soci_stream *soci_stream_new(void) {
    soci_stream *s = calloc(1, sizeof(*s));
    if (s && inflateInit2(&s->z, 47) != Z_OK) { free(s); return NULL; }
    return s;
}
static int soci_stream_reset(soci_stream *s) { return inflateReset2(&s->z, 47); }
static void soci_stream_free(soci_stream *s) { inflateEnd(&s->z); free(s); }
static int soci_stream_step(soci_stream *s, unsigned char *input, unsigned size, uint64_t span) {
    if (s->used == 32768) s->used = 0;
    unsigned room = 32768 - s->used;
    s->z.next_in = input;
    s->z.avail_in = size;
    s->z.next_out = s->window + s->used;
    s->z.avail_out = room;
    int ret = inflate(&s->z, Z_BLOCK);
    s->consumed = size - s->z.avail_in;
    s->produced = room - s->z.avail_out;
    s->used += s->produced;
    s->in += s->consumed;
    s->out += s->produced;
    s->bits = s->z.data_type & 7;
    s->checkpoint = ret != Z_STREAM_END && (s->z.data_type & 128) && !(s->z.data_type & 64)
        && (s->out == 0 || s->out - s->last > span);
    if (s->checkpoint) s->last = s->out;
    // Never retain a Go input pointer across cgo calls.
    s->z.next_in = NULL;
    s->z.avail_in = 0;
    return ret;
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"io"
	"unsafe"

	"github.com/opencontainers/go-digest"
)

// GzipStream emits tar bytes while constructing SOCI-compatible checkpoints and
// span digests. Memory is bounded by two 32 KiB buffers plus the resulting index.
type GzipStream struct {
	s               *C.soci_stream
	r               io.Reader
	span            int64
	buffer          [32768]byte
	input, output   []byte
	inputErr        error
	memberEnd, done bool
	last            byte
	hash            digest.Digester
	Digests         []digest.Digest
	Checkpoints     []byte
	Size            Offset
}

func NewGzipStream(r io.Reader, span int64) (*GzipStream, error) {
	if span <= 0 {
		return nil, fmt.Errorf("invalid span size %d", span)
	}
	s := C.soci_stream_new()
	if s == nil {
		return nil, fmt.Errorf("cannot allocate gzip stream")
	}
	return &GzipStream{s: s, r: r, span: span, Checkpoints: make([]byte, 12)}, nil
}

func (r *GzipStream) Close() error {
	if r.s != nil {
		C.soci_stream_free(r.s)
		r.s = nil
	}
	return nil
}

func (r *GzipStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.output) == 0 && !r.done {
		if len(r.input) == 0 && r.inputErr == nil {
			n, err := r.r.Read(r.buffer[:])
			r.input, r.inputErr = r.buffer[:n], err
		}
		if len(r.input) == 0 {
			if r.inputErr == nil {
				continue
			}
			if r.inputErr != io.EOF {
				return 0, r.inputErr
			}
			if !r.memberEnd || r.hash == nil {
				return 0, io.ErrUnexpectedEOF
			}
			r.Digests = append(r.Digests, r.hash.Digest())
			binary.LittleEndian.PutUint32(r.Checkpoints, uint32(len(r.Digests)))
			binary.LittleEndian.PutUint64(r.Checkpoints[4:], uint64(r.span))
			r.Size = Offset(r.s.in)
			r.done = true
			break
		}
		if r.memberEnd {
			if ret := C.soci_stream_reset(r.s); ret != C.Z_OK {
				return 0, fmt.Errorf("reset gzip: %d", ret)
			}
			r.memberEnd = false
		}
		ret := C.soci_stream_step(r.s, (*C.uchar)(unsafe.Pointer(&r.input[0])), C.uint(len(r.input)), C.uint64_t(r.span))
		if ret != C.Z_OK && ret != C.Z_STREAM_END {
			return 0, fmt.Errorf("index gzip: %d", ret)
		}
		n := int(r.s.consumed)
		if r.hash != nil {
			r.hash.Hash().Write(r.input[:n])
		}
		if n > 0 {
			r.last = r.input[n-1]
		}
		r.input = r.input[n:]
		if r.s.checkpoint != 0 {
			if r.hash != nil {
				r.Digests = append(r.Digests, r.hash.Digest())
			}
			r.hash = digest.Canonical.Digester()
			if r.s.bits != 0 {
				r.hash.Hash().Write([]byte{r.last})
			}
			r.Checkpoints = binary.LittleEndian.AppendUint64(r.Checkpoints, uint64(r.s.in))
			r.Checkpoints = binary.LittleEndian.AppendUint64(r.Checkpoints, uint64(r.s.out))
			r.Checkpoints = append(r.Checkpoints, byte(r.s.bits))
			window := unsafe.Slice((*byte)(unsafe.Pointer(&r.s.window[0])), 32768)
			r.Checkpoints = append(r.Checkpoints, window[int(r.s.used):]...)
			r.Checkpoints = append(r.Checkpoints, window[:int(r.s.used)]...)
		}
		if r.s.produced > 0 {
			window := unsafe.Slice((*byte)(unsafe.Pointer(&r.s.window[0])), 32768)
			r.output = window[int(r.s.used-r.s.produced):int(r.s.used)]
		}
		r.memberEnd = ret == C.Z_STREAM_END
	}
	if len(r.output) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.output)
	r.output = r.output[n:]
	return n, nil
}
