//go:build socihelper

package ztoc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

type fragmentedReader struct{ io.Reader }

func (r fragmentedReader) Read(p []byte) (int, error) {
	if len(p) > 73 {
		p = p[:73]
	}
	return r.Reader.Read(p)
}

func TestStreamingZtocCompatibility(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	random := make([]byte, 300001)
	rand.New(rand.NewSource(1)).Read(random)
	for i, payload := range [][]byte{random, bytes.Repeat([]byte("abcdef"), 100003)} {
		if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("payload-%d", i), Mode: 0644, Size: int64(len(payload)), Format: tar.FormatPAX, PAXRecords: map[string]string{"SCHILY.xattr.user.example": "value"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "payload-0"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, split := range []int{archive.Len(), 139777} {
		t.Run(fmt.Sprintf("member-size-%d", split), func(t *testing.T) {
			var compressed bytes.Buffer
			for offset := 0; offset < archive.Len(); offset += split {
				w, err := gzip.NewWriterLevel(&compressed, 3)
				if err != nil {
					t.Fatal(err)
				}
				w.Name = "non-default-gzip-header"
				if _, err := w.Write(archive.Bytes()[offset:min(offset+split, archive.Len())]); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), "layer.gz")
			if err := os.WriteFile(path, compressed.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			builder := NewBuilder("stream-test")
			want, err := builder.BuildZtoc(path, 32768)
			if err != nil {
				t.Fatal(err)
			}
			if want.MaxSpanID < 3 {
				t.Fatal("fixture did not cross enough span boundaries")
			}
			got, err := builder.BuildZtocReader(fragmentedReader{bytes.NewReader(compressed.Bytes())}, 32768)
			if err != nil {
				t.Fatal(err)
			}
			_, wantDesc, err := Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			_, gotDesc, err := Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if wantDesc.Digest != gotDesc.Digest {
				t.Fatalf("streaming zTOC differs from upstream: want %s, got %s", wantDesc.Digest, gotDesc.Digest)
			}
			for _, cut := range []int{1, 8, compressed.Len() / 2} {
				if _, err := builder.BuildZtocReader(bytes.NewReader(compressed.Bytes()[:compressed.Len()-cut]), 32768); err == nil {
					t.Fatalf("accepted truncation of %d bytes", cut)
				}
			}
			corrupt := bytes.Clone(compressed.Bytes())
			corrupt[len(corrupt)-8] ^= 1
			if _, err := builder.BuildZtocReader(bytes.NewReader(corrupt), 32768); err == nil {
				t.Fatal("accepted bad gzip checksum")
			}
		})
	}
}
