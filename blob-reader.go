package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

type blobReader struct {
	s   *blobState
	f   *os.File
	pos uint64
}

func (s *blobState) Reader() (io.ReadCloser, error) {
	path := blobPath(s.digest)

	if s.fetching {
		path += ".part"
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &blobReader{s: s, f: f, pos: 0}, nil
}

func (r *blobReader) Read(b []byte) (n int, err error) {
	if r.pos == r.s.length {
		return 0, io.EOF
	}

	r.s.c.L.Lock()
	for r.pos >= r.s.fetchPos && r.s.code == 200 {
		r.s.c.Wait()
	}
	r.s.c.L.Unlock()

	if r.s.code != 200 {
		return 0, fmt.Errorf("fetch failed: %s", http.StatusText(r.s.code))
	}

	n, err = r.f.Read(b)
	r.pos += uint64(n)

	if err == io.EOF && r.pos < r.s.length {
		// there will be more
		err = nil
	}

	return
}

func (r *blobReader) Close() error {
	return r.f.Close()
}
