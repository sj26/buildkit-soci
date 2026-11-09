//go:build socihelper

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/awslabs/soci-snapshotter/ztoc"
)

// Built in SOCI's pinned source tree, keeping native dependencies out of buildkitd.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: buildkit-soci-ztoc layer.gz")
	}
	toc, err := ztoc.NewBuilder("BuildKit SOCI spike / soci v0.16.1").BuildZtoc(os.Args[1], 4<<20)
	if err != nil {
		return err
	}
	data, _, err := ztoc.Marshal(toc)
	if err != nil {
		return err
	}
	_, err = io.Copy(os.Stdout, data)
	return err
}
