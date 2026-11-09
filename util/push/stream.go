package push

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/resolver/limited"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

const uploadChunkSize = 10 << 20

// StreamUpload sends new gzip bytes before their final digest is known. It is
// speculative: Commit reports errors, and the ordinary finalized-blob push can
// retry from the content store. No manifests or tags are published here.
type StreamUpload struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	pusher                  *BlobPusher
	chunks                  chan []byte
	done                    chan struct{}
	err                     error
	buffer                  []byte
	sent, closed, committed bool
	host                    docker.RegistryHost
	location                *url.URL
	offset                  int64
}

func (p *BlobPusher) Start(ctx context.Context) *StreamUpload {
	ctx = docker.WithScope(ctx, "repository:"+reference.Path(p.ref)+":pull,push")
	ctx, cancel := context.WithCancel(ctx)
	u := &StreamUpload{ctx: ctx, cancel: cancel, pusher: p, chunks: make(chan []byte, 1), done: make(chan struct{})}
	go func() {
		defer close(u.done)
		u.err = u.run()
		if u.err != nil {
			cancel()
		}
	}()
	return u
}

func (u *StreamUpload) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && u.ctx.Err() == nil {
		if u.buffer == nil {
			u.buffer = make([]byte, 0, uploadChunkSize)
		}
		k := min(len(p), uploadChunkSize-len(u.buffer))
		u.buffer = append(u.buffer, p[:k]...)
		p = p[k:]
		if len(u.buffer) == uploadChunkSize {
			u.enqueue()
		}
	}
	// Upload failure must not interrupt the local content-store write.
	return n, nil
}

func (u *StreamUpload) enqueue() {
	select {
	case u.chunks <- u.buffer:
		u.sent = true
	case <-u.ctx.Done():
	}
	u.buffer = nil
}

func (u *StreamUpload) run() (retErr error) {
	select {
	case first := <-u.chunks:
		if first == nil {
			return nil
		}
		done := progress.OneOff(u.ctx, "uploading gzip during compression")
		defer func() { done(retErr) }()
		hosts, err := u.pusher.resolver.HostsFunc(reference.Domain(u.pusher.ref))
		if err != nil {
			return err
		}
		for _, h := range hosts {
			if h.Capabilities&docker.HostCapabilityPush != 0 {
				u.host = h
				break
			}
		}
		if u.host.Host == "" {
			return fmt.Errorf("no registry push host")
		}
		start, err := url.Parse(u.host.Scheme + "://" + u.host.Host + strings.TrimSuffix(u.host.Path, "/") + "/" + reference.Path(u.pusher.ref) + "/blobs/uploads/")
		if err != nil {
			return err
		}
		resp, err := u.request(u.ctx, http.MethodPost, start, nil, http.StatusAccepted)
		if err != nil {
			return err
		}
		if resp.Header.Get("Location") == "" {
			return fmt.Errorf("registry omitted upload location")
		}
		if minSize, _ := strconv.ParseInt(resp.Header.Get("OCI-Chunk-Min-Length"), 10, 64); minSize > uploadChunkSize {
			return fmt.Errorf("registry minimum chunk size %d exceeds %d", minSize, uploadChunkSize)
		}
		if err := u.patch(first); err != nil {
			return err
		}
	case <-u.ctx.Done():
		return u.ctx.Err()
	}
	for {
		select {
		case data, ok := <-u.chunks:
			if !ok {
				return nil
			}
			if err := u.patch(data); err != nil {
				return err
			}
		case <-u.ctx.Done():
			return u.ctx.Err()
		}
	}
}

func (u *StreamUpload) patch(data []byte) error {
	if _, err := u.request(u.ctx, http.MethodPatch, u.location, data, http.StatusAccepted, http.StatusCreated, http.StatusNoContent); err != nil {
		return err
	}
	u.offset += int64(len(data))
	return nil
}

func (u *StreamUpload) Commit(desc ocispecs.Descriptor) error {
	// Small layers use the normal push, which can first check for an existing blob.
	if !u.sent {
		return nil
	}
	if len(u.buffer) > 0 {
		u.enqueue()
	}
	close(u.chunks)
	u.closed = true
	<-u.done
	if u.err != nil {
		return u.err
	}
	if u.offset != desc.Size {
		return fmt.Errorf("stream upload size %d differs from blob size %d", u.offset, desc.Size)
	}
	location := *u.location
	query := location.Query()
	query.Set("digest", desc.Digest.String())
	location.RawQuery = query.Encode()
	resp, err := u.request(u.ctx, http.MethodPut, &location, nil, http.StatusCreated)
	if err != nil {
		return err
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != desc.Digest.String() {
		return fmt.Errorf("registry committed an unexpected digest")
	}
	u.committed = true
	return nil
}

func (u *StreamUpload) Close() {
	u.cancel()
	if !u.closed {
		close(u.chunks)
		u.closed = true
	}
	<-u.done
	if u.location != nil && !u.committed {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), 2*time.Second)
		defer cancel()
		u.request(ctx, http.MethodDelete, u.location, nil, http.StatusNoContent)
	}
}

func (u *StreamUpload) request(ctx context.Context, method string, location *url.URL, data []byte, statuses ...int) (*http.Response, error) {
	ctx, release, err := limited.Default.Acquire(ctx, u.pusher.ref.String(), ocispecs.Descriptor{})
	if err != nil {
		return nil, err
	}
	defer release()
	// Upload locations can move to storage hosts. Do not forward registry auth,
	// custom headers or follow redirects automatically across that boundary.
	sameOrigin := location.Host == u.host.Host && location.Scheme == u.host.Scheme
	client := *u.host.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, location.String(), bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if sameOrigin {
			for k, values := range u.host.Header {
				req.Header[k] = append([]string(nil), values...)
			}
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		if method == http.MethodPatch {
			req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", u.offset, u.offset+int64(len(data))-1))
		}
		if sameOrigin && u.host.Authorizer != nil {
			if err := u.host.Authorizer.Authorize(ctx, req); err != nil {
				return nil, err
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && sameOrigin && u.host.Authorizer != nil && attempt == 0 {
			if err := u.host.Authorizer.AddResponses(ctx, []*http.Response{resp}); err != nil {
				return nil, err
			}
			continue
		}
		if !slices.Contains(statuses, resp.StatusCode) {
			return nil, fmt.Errorf("registry %s returned %d, expected %v", method, resp.StatusCode, statuses)
		}
		if next := resp.Header.Get("Location"); next != "" && (method == http.MethodPost || method == http.MethodPatch) {
			u.location, err = location.Parse(next)
			if err != nil {
				return nil, err
			}
		}
		return resp, nil
	}
	return nil, fmt.Errorf("registry authentication failed")
}
