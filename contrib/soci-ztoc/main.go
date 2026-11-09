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
		return fmt.Errorf("usage: buildkit-soci-ztoc layer.gz|-")
	}
	input := os.Stdin
	if os.Args[1] != "-" {
		var err error
		input, err = os.Open(os.Args[1])
		if err != nil {
			return err
		}
		defer input.Close()
	}
	toc, err := ztoc.NewBuilder("BuildKit SOCI spike / soci v0.16.1").BuildZtocReader(input, 4<<20)
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
