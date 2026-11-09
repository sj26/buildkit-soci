//go:build socihelper

package main

import (
	"fmt"
	"io"
	"os"
	"strconv"

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
	if len(os.Args) == 3 && os.Args[1] == "--compress" {
		level, err := strconv.Atoi(os.Args[2])
		if err != nil {
			return err
		}
		toc, err := ztoc.NewBuilder("BuildKit SOCI spike / soci v0.16.1").CompressZtoc(os.Stdin, os.Stdout, 4<<20, level)
		if err != nil {
			return err
		}
		output := os.NewFile(3, "ztoc")
		defer output.Close()
		return writeZtoc(output, toc)
	}
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: buildkit-soci-ztoc layer.gz|-|--compress level")
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
	return writeZtoc(os.Stdout, toc)
}

func writeZtoc(output io.Writer, toc *ztoc.Ztoc) error {
	data, _, err := ztoc.Marshal(toc)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, data)
	return err
}
