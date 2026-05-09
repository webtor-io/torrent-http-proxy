package services

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/urfave/cli"
)

// fileKeyCtxKey carries the resolved (infoHash, path) of a request so that
// response-side hooks (notably FileSizeCache population in modifyResponse)
// can key per-file state without re-parsing the URL — by the time
// modifyResponse runs, the proxy Director has rewritten r.Request.URL to
// point at the upstream and the original client path is gone.
type fileKeyCtxKey struct{}

type FileKey struct {
	InfoHash string
	Path     string
}

func WithFileKey(r *http.Request, infoHash, path string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), fileKeyCtxKey{}, &FileKey{infoHash, path}))
}

func GetFileKey(r *http.Request) *FileKey {
	v, _ := r.Context().Value(fileKeyCtxKey{}).(*FileKey)
	return v
}

const (
	FileSizeCacheCapacityFlag = "file-size-cache-capacity"
)

func RegisterFileSizeCacheFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   FileSizeCacheCapacityFlag,
			Usage:  "max entries in the upstream file-size cache (random-evict on overflow)",
			Value:  100000,
			EnvVar: "FILE_SIZE_CACHE_CAPACITY",
		},
	)
}

// FileSizeCache remembers the upstream byte size of (infoHash, path) tuples
// learned from response headers. SessionLimiter consults it to decide
// whether a path is "big" (counts toward the per-hash cap) or "light"
// (passes freely). File sizes don't change for the lifetime of a torrent
// piece-store, so entries are kept indefinitely up to capacity; on
// overflow we drop ~10% of entries at random — there's no usage-pattern
// signal to LRU on, and the cost of a re-population is one upstream
// response we'd see anyway.
type FileSizeCache struct {
	capacity int
	mu       sync.RWMutex
	m        map[string]int64
}

func NewFileSizeCache(c *cli.Context) *FileSizeCache {
	cap := c.Int(FileSizeCacheCapacityFlag)
	if cap <= 0 {
		cap = 100000
	}
	return &FileSizeCache{
		capacity: cap,
		m:        make(map[string]int64, cap/4),
	}
}

func cacheKey(infoHash, path string) string {
	return infoHash + "|" + path
}

func (c *FileSizeCache) Get(infoHash, path string) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n, ok := c.m[cacheKey(infoHash, path)]
	return n, ok
}

func (c *FileSizeCache) Set(infoHash, path string, size int64) {
	if size <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.capacity {
		toEvict := c.capacity / 10
		if toEvict < 1 {
			toEvict = 1
		}
		for k := range c.m {
			delete(c.m, k)
			toEvict--
			if toEvict <= 0 {
				break
			}
		}
	}
	c.m[cacheKey(infoHash, path)] = size
}

// SizeFromHeaders extracts the underlying file size from an upstream
// response. For HTTP 200 we trust Content-Length verbatim. For HTTP 206
// (range) we parse the total off Content-Range ("bytes X-Y/TOTAL").
// Returns 0 when the size can't be determined — callers should treat
// that as "unknown" and not Set the cache.
func SizeFromHeaders(statusCode int, contentLength int64, contentRange string) int64 {
	if statusCode == 206 {
		if i := strings.LastIndex(contentRange, "/"); i >= 0 {
			tail := contentRange[i+1:]
			if tail != "*" {
				if n, err := strconv.ParseInt(tail, 10, 64); err == nil && n > 0 {
					return n
				}
			}
		}
		return 0
	}
	if statusCode == 200 && contentLength > 0 {
		return contentLength
	}
	return 0
}
