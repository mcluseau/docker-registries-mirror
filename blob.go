package main

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"flag"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cespare/xxhash"
	"github.com/twmb/murmur3"
)

var (
	peers = flag.String("peers", "", "peers to ask for missing blobs")

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

func serveBlob(w http.ResponseWriter, req *http.Request, blob string, failIfMissing bool) {
	state := getState(blob, req, failIfMissing)

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

func getState(blob string, req *http.Request, failIfMissing bool) (state *blobState) {
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

	if failIfMissing {
		state.code = http.StatusNotFound
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

	var resp *http.Response
	if *peers != "" {
		for _, peer := range strings.Split(*peers, ",") {
			resp, err = http.Get(peer + "/blobs/" + blob)
			if err != nil {
				log.Print("warning: peer ", peer, " failed: ", err)
				continue
			}

			if resp.StatusCode == 200 {
				log.Print("found blob ", blob, " on peer ", peer)
				break
			}

			resp = nil
		}
	}

	if resp == nil {
		log.Print("fetching blob ", state.digest, " from ", req.URL)

		proxyReq, err := http.NewRequest("GET", req.URL.String(), nil)
		if err != nil {
			log.Fatal("bad new request: ", req.URL)
		}

		// copy some headers
		for _, hdr := range []string{"Accept", "Authorization"} {
			proxyReq.Header.Set(hdr, req.Header.Get(hdr))
		}

		resp, err = http.DefaultClient.Do(proxyReq)
		if err != nil {
			log.Print(" -> fetch failed: ", err)
			state.code = http.StatusBadGateway
			return
		}
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

	var (
		alg, checksum string
		h             hash.Hash
	)

	digestParts := strings.SplitN(state.digest, ":", 2)
	if len(digestParts) == 2 {
		alg, checksum = digestParts[0], digestParts[1]
	}

	switch alg {
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()

	case "md5":
		h = md5.New()

	case "xx":
		h = xxhash.New()

	case "murmur32":
		h = murmur3.New32()
	case "murmur64":
		h = murmur3.New64()
	case "murmur128":
		h = murmur3.New128()

	default:
		log.Print("warning: unknown hash algorithm, will not check download: ", alg)
	}

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)

		if n == 0 && err == io.EOF {
			break // finished
		}

		if err == io.EOF {
			err = nil
		}

		if h != nil {
			h.Write(buf[:n])
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

	if h == nil {
		return true
	}

	myChecksum := hex.EncodeToString(h.Sum(nil))
	if checksum == myChecksum {
		return true
	}

	log.Print("error: wrong checksum: expected ", checksum, " got ", myChecksum)
	return false
}
