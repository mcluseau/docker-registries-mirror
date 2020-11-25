package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
