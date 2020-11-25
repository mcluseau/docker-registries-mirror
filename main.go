package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	bind     = flag.String("addr", ":80", "bind address")
	cacheDir = flag.String("cache", "cache", "cache dir")

	rp = &httputil.ReverseProxy{
		Director: func(_ *http.Request) {},
	}

	blobRE = regexp.MustCompile("/blobs/([[:alnum:]]+:[0-9a-f]+)$")
)

func main() {
	flag.Parse()

	for _, sub := range []string{"blobs"} {
		if err := os.MkdirAll(filepath.Join(*cacheDir, sub), 0755); err != nil {
			log.Fatal("failed to create cache dir: ", err)
		}
	}

	if *cacheSize != 0 {
		go cacheCleaner()
	}

	log.Print("listening on ", *bind)
	http.ListenAndServe(*bind, handler{})
}

type handler struct{}

func (_ handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	log := log.New(os.Stderr, req.RemoteAddr+": ", log.Flags())

	u := req.URL

	path := u.Path

	if path[0] == '/' {
		path = path[1:]
	}

	idx := strings.IndexByte(path, '/')
	if idx == -1 {
		w.Write([]byte("registries mirror\n"))
		return
	}

	scheme := path[:idx]
	path = path[idx+1:]

	idx = strings.IndexByte(path, '/')
	if idx == -1 {
		u.Path = u.Path + "/"
		http.Redirect(w, req, u.Path, http.StatusPermanentRedirect)
		return
	} else if idx == 0 {
		http.NotFound(w, req)
		return
	}

	host := path[:idx]
	path = path[idx:]

	u.Scheme = scheme
	u.Host = host
	req.Host = host

	u.Path = path

	if req.Method == http.MethodGet {
		if m := blobRE.FindStringSubmatch(path); m != nil {
			blob := m[1]
			log.Print("serving blob ", blob)
			serveBlob(w, req, blob)
			return
		}
	}

	log.Print("proxying ", u)
	rp.ServeHTTP(w, req)
}

var (
	blobLock   = &sync.Mutex{}
	blobStates = map[string]*blobState{}
)

type blobState struct {
	c        *sync.Cond
	fetching bool

	length uint64
	digest string

	fetchPos uint64
	code     int
	failed   bool

	out *os.File
}

func blobPath(digest string) string {
	return filepath.Join(*cacheDir, "blobs", digest)
}

func serveBlob(w http.ResponseWriter, req *http.Request, blob string) {
	state := getState(blob, req)

	if state.code != 200 {
		http.Error(w, http.StatusText(state.code), state.code)
		return
	}

	in, err := state.Reader()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		log.Print("failed to read blob ", blob, ": ", err)
		return
	}

	defer in.Close()

	hdr := w.Header()
	hdr.Set("Content-Length", strconv.FormatUint(state.length, 10))
	hdr.Set("Docker-Content-Digest", blob)
	hdr.Set("Content-Type", "application/octet-stream")

	io.Copy(w, in)
}

func getState(blob string, req *http.Request) (state *blobState) {
	blobLock.Lock()
	defer blobLock.Unlock()

	state = blobStates[blob]
	if state != nil {
		return
	}

	state = &blobState{c: sync.NewCond(&sync.Mutex{}), digest: blob}

	blobPath := blobPath(blob)

	stat, err := os.Stat(blobPath)
	if err == nil {
		// file exists
		state.length = uint64(stat.Size())
		state.fetchPos = state.length
		state.code = 200

		if stat, ok := stat.Sys().(*syscall.Stat_t); ok {
			if time.Since(time.Unix(stat.Atim.Sec, 0)) > time.Minute {
				os.Chtimes(blobPath, time.Now(), time.Unix(0, stat.Mtim.Nano()))
			}
		}

		return

	} else if !os.IsNotExist(err) {
		log.Print("failed to stat blob: ", err)
		state.code = http.StatusBadGateway
		return
	}

	blobStates[blob] = state

	defer func() {
		if state.code != 200 {
			delete(blobStates, blob)
		}
	}()

	out, err := os.Create(blobPath + ".part")
	if err != nil {
		log.Print("failed to create blob: ", err)
		state.code = http.StatusBadGateway
		return
	}

	defer func() {
		if !state.fetching {
			log.Print("failed before fetch, clearing ", out.Name())
			out.Close()
			os.Remove(out.Name())
		}
	}()

	log.Print("fetching blob ", state.digest, " from ", req.URL)

	proxyReq, err := http.NewRequest("GET", req.URL.String(), nil)
	if err != nil {
		log.Fatal("bad new request: ", req.URL)
	}

	// copy some headers
	for _, hdr := range []string{"Accept", "Authorization"} {
		proxyReq.Header.Set(hdr, req.Header.Get(hdr))
	}

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		log.Print(" -> fetch failed: ", err)
		state.code = http.StatusBadGateway
		return
	}

	state.code = resp.StatusCode

	if state.code != 200 {
		out.Close()
		resp.Body.Close()
		return
	}

	state.length, err = strconv.ParseUint(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		log.Print("invalid bad length returned by remote: ", resp.Header.Get("Content-Length"), ": ", err)
		state.code = http.StatusBadGateway
		resp.Body.Close()
		return
	}

	state.fetching = true
	go func() {
		if fetchBlob(out, resp, state) {
			os.Rename(blobPath+".part", blobPath)
		}
	}()

	return
}

func fetchBlob(out *os.File, resp *http.Response, state *blobState) (ok bool) {
	defer resp.Body.Close()
	defer out.Close()

	defer func() {
		state.c.L.Lock()
		state.fetching = false
		state.c.L.Unlock()
		state.c.Broadcast()
	}()

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)

		if n == 0 && err == io.EOF {
			return true // finished
		}

		if err == io.EOF {
			err = nil
		}

		if err == nil {
			n, err = out.Write(buf[:n])
			if err != nil {
				log.Print("failed to write to blob ", state.digest, ": ", err)
			}
		} else {
			log.Print("blob ", state.digest, ": failed to read from remote: ", err)
		}

		state.c.L.Lock()
		state.fetchPos += uint64(n)
		if err != nil {
			state.failed = true
		}
		state.c.L.Unlock()
		state.c.Broadcast()

		if err != nil {
			blobLock.Lock()
			defer blobLock.Unlock()

			delete(blobStates, state.digest)

			if err := os.Remove(out.Name()); err != nil {
				log.Print("failed to delete ", out.Name(), ": ", err)
			}
			return false // failed
		}
	}
}

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
