package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/x/plugin"
	gitzlib "github.com/go-git/go-git/v6/x/plugin/zlib"
	kpzlib "github.com/klauspost/compress/zlib"
)

var goGitKlauspostZlib = flag.Bool("gogit-klauspost-zlib", false, "use klauspost zlib for go-git object decompression")

func configureGoGitZlib() error {
	if !*goGitKlauspostZlib {
		return nil
	}
	return plugin.Register(plugin.Zlib(), func() plugin.ZlibProvider {
		return klauspostZlibProvider{}
	})
}

type klauspostZlibProvider struct{}

func (klauspostZlibProvider) NewReader(r io.Reader) (gitzlib.Reader, error) {
	reader, err := kpzlib.NewReader(r)
	if err != nil {
		return nil, err
	}
	resettable, ok := reader.(gitzlib.Reader)
	if !ok {
		_ = reader.Close()
		return nil, fmt.Errorf("klauspost zlib reader %T does not implement reset", reader)
	}
	return resettable, nil
}

func (klauspostZlibProvider) NewWriter(w io.Writer) gitzlib.Writer {
	return kpzlib.NewWriter(w)
}
