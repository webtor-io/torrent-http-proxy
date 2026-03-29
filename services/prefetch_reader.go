package services

import (
	"io"
	"sync"
)

// PrefetchReader wraps an io.ReadCloser with a background goroutine that
// eagerly reads ahead into a fixed-size ring buffer. This smooths out
// bursty source delivery (e.g. torrent peers, S3) by always having data
// ready for the consumer.
type PrefetchReader struct {
	src    io.ReadCloser
	buf    []byte
	size   int
	r      int
	count  int
	mu     sync.Mutex
	cond   *sync.Cond
	done   bool
	err    error
	closed bool
}

func NewPrefetchReader(src io.ReadCloser, bufSize int) *PrefetchReader {
	p := &PrefetchReader{
		src:  src,
		buf:  make([]byte, bufSize),
		size: bufSize,
	}
	p.cond = sync.NewCond(&p.mu)
	go p.fill()
	return p
}

func (p *PrefetchReader) fill() {
	tmp := make([]byte, 64<<10)
	for {
		n, err := p.src.Read(tmp)

		p.mu.Lock()
		if n > 0 {
			copied := 0
			for copied < n {
				for p.count == p.size && !p.closed {
					p.cond.Wait()
				}
				if p.closed {
					p.mu.Unlock()
					return
				}
				w := (p.r + p.count) % p.size
				space := p.size - p.count
				contiguous := p.size - w
				toWrite := n - copied
				if toWrite > space {
					toWrite = space
				}
				if toWrite > contiguous {
					toWrite = contiguous
				}
				copy(p.buf[w:w+toWrite], tmp[copied:copied+toWrite])
				p.count += toWrite
				copied += toWrite
				p.cond.Broadcast()
			}
		}
		if err != nil {
			p.done = true
			p.err = err
			p.cond.Broadcast()
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
}

func (p *PrefetchReader) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for p.count == 0 && !p.done {
		p.cond.Wait()
	}

	if p.count == 0 {
		if p.err != nil {
			return 0, p.err
		}
		return 0, io.EOF
	}

	n := len(dst)
	if n > p.count {
		n = p.count
	}
	contiguous := p.size - p.r
	if n <= contiguous {
		copy(dst[:n], p.buf[p.r:p.r+n])
	} else {
		copy(dst[:contiguous], p.buf[p.r:p.size])
		copy(dst[contiguous:n], p.buf[:n-contiguous])
	}
	p.r = (p.r + n) % p.size
	p.count -= n
	p.cond.Broadcast()

	return n, nil
}

func (p *PrefetchReader) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	return p.src.Close()
}
