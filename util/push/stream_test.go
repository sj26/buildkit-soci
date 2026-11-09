package push

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestStreamUpload(t *testing.T) {
	for _, mode := range []string{"complete", "failed-patch", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var received bytes.Buffer
			var committed, deleted bool
			patched := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case http.MethodPost:
					// ECR advertises this minimum on the upload-session response.
					w.Header().Set("OCI-Chunk-Min-Length", "10485760")
					w.Header().Set("Location", "/upload?state=opaque")
					w.WriteHeader(http.StatusAccepted)
				case http.MethodPatch:
					defer func() {
						select {
						case patched <- struct{}{}:
						default:
						}
					}()
					if mode == "failed-patch" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					want := fmt.Sprintf("%d-%d", received.Len(), int64(received.Len())+r.ContentLength-1)
					if r.Header.Get("Content-Range") != want || r.URL.Query().Get("state") != "opaque" {
						t.Errorf("invalid chunk range or lost upload parameters: %s", r.Header.Get("Content-Range"))
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					io.Copy(&received, r.Body)
					w.Header().Set("Location", "/upload?state=opaque")
					w.WriteHeader(http.StatusAccepted)
				case http.MethodPut:
					io.Copy(&received, r.Body)
					if r.URL.Query().Get("digest") != digest.FromBytes(received.Bytes()).String() {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					committed = true
					w.Header().Set("Docker-Content-Digest", r.URL.Query().Get("digest"))
					w.WriteHeader(http.StatusCreated)
				case http.MethodDelete:
					deleted = true
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			sm, err := session.NewManager()
			if err != nil {
				t.Fatal(err)
			}
			host := strings.TrimPrefix(server.URL, "http://")
			pusher, err := NewBlobPusher(context.Background(), sm, "", host+"/test:tag", false, func(string) ([]docker.RegistryHost, error) {
				return []docker.RegistryHost{{Client: server.Client(), Host: host, Scheme: "http", Path: "/v2", Capabilities: docker.HostCapabilityPush}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			upload := pusher.Start(ctx)
			data := bytes.Repeat([]byte("chunked-stream"), 900013)
			if _, err := upload.Write(data[:uploadChunkSize]); err != nil {
				t.Fatal(err)
			}
			select {
			case <-patched:
			case <-ctx.Done():
				t.Fatal("no bytes uploaded before producer completion")
			}
			mu.Lock()
			if committed {
				t.Error("committed before final digest was known")
			}
			mu.Unlock()
			if mode == "cancel" {
				cancel()
			} else {
				if n, err := upload.Write(data[uploadChunkSize:]); err != nil || n != len(data)-uploadChunkSize {
					t.Fatalf("local producer interrupted: %d, %v", n, err)
				}
				err := upload.Commit(ocispecs.Descriptor{Digest: digest.FromBytes(data), Size: int64(len(data))})
				if (err != nil) != (mode == "failed-patch") {
					t.Fatalf("commit: %v", err)
				}
			}
			upload.Close()
			mu.Lock()
			if mode == "complete" {
				if !committed || deleted || !bytes.Equal(received.Bytes(), data) {
					t.Fatal("incorrect committed blob")
				}
			} else if committed || !deleted {
				t.Fatal("failed/canceled upload was not aborted")
			}
			mu.Unlock()
			if mode == "failed-patch" {
				store := contentutil.NewBuffer()
				desc := ocispecs.Descriptor{MediaType: ocispecs.MediaTypeImageLayerGzip, Digest: digest.FromBytes(data), Size: int64(len(data))}
				if err := content.WriteBlob(ctx, store, desc.Digest.String(), bytes.NewReader(data), desc); err != nil {
					t.Fatal(err)
				}
				if err := pusher.Push(ctx, desc, store); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				defer mu.Unlock()
				if !committed || !bytes.Equal(received.Bytes(), data) {
					t.Fatal("finalized-blob fallback did not publish identical bytes")
				}
			}
		})
	}
}
