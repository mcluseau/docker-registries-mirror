package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

var (
	cacheSize          = flag.Int64("cache-mib", 0, "keep cache under this size (in MiB)")
	cacheCheckInterval = flag.Duration("cache-check", time.Hour, "interval between cache checks")
)

func cacheCleaner() {
	type fileInfo struct {
		atime time.Time
		path  string
		size  int64
	}

	cacheSize := (*cacheSize) << 20

	infos := make([]fileInfo, 0)

	for range time.Tick(time.Second) {
		infos = infos[:0]
		var size int64

		filepath.Walk(*cacheDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			size += info.Size()

			s := info.Sys().(*syscall.Stat_t)
			infos = append(infos, fileInfo{
				path:  path,
				size:  info.Size(),
				atime: time.Unix(s.Atim.Sec, 0),
			})

			return nil
		})

		if size > cacheSize {
			// sort by atime (then size and path)
			sort.Slice(infos, func(i, j int) bool {
				a, b := infos[i], infos[j]

				if !a.atime.Equal(b.atime) {
					return a.atime.Before(b.atime) // older first
				}

				if a.size != b.size {
					return b.size < a.size // bigger first
				}

				return a.path < b.path
			})

			for _, info := range infos {
				log.Print("clearing cache entry ", info.path)
				if err := os.Remove(info.path); err != nil {
					log.Print("failed to remove ", info.path, ": ", err)
					continue
				}

				size -= info.size

				if size <= cacheSize {
					break
				}
			}
		}
	}
}
