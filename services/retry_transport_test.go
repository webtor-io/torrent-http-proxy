package services

import (
	"io"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

// dirtyReader delivers data, then fails with a retryable error instead of EOF.
type dirtyReader struct {
	data io.Reader
	err  error
}

func (d *dirtyReader) Read(p []byte) (int, error) {
	n, err := d.data.Read(p)
	if err == io.EOF {
		return n, d.err
	}
	return n, err
}

func (d *dirtyReader) Close() error { return nil }

func newTestRRC(body io.ReadCloser, expected int64, reconnectFn func(int64) (io.ReadCloser, error)) *retryingReadCloser {
	return &retryingReadCloser{
		body:        body,
		reconnectFn: reconnectFn,
		expected:    expected,
		maxRetries:  2,
		logger:      logrus.WithField("test", true),
	}
}

func TestRetryFullyDeliveredDirtyCloseIsEOF(t *testing.T) {
	payload := "0123456789"
	body := &dirtyReader{data: strings.NewReader(payload), err: io.ErrUnexpectedEOF}
	reconnects := 0
	r := newTestRRC(body, int64(len(payload)), func(offset int64) (io.ReadCloser, error) {
		reconnects++
		return nil, errUpstreamEOF
	})
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("expected clean EOF, got %v", err)
	}
	if string(got) != payload {
		t.Fatalf("payload mismatch: %q", got)
	}
	if reconnects != 0 {
		t.Fatalf("expected no reconnects when Content-Length is known, got %d", reconnects)
	}
}

func TestRetryReconnect416AtSizeIsEOF(t *testing.T) {
	// Unknown Content-Length: the reader must probe via reconnect, and an
	// errUpstreamEOF sentinel (416 with matching Content-Range total) must
	// surface as clean EOF.
	payload := "0123456789"
	body := &dirtyReader{data: strings.NewReader(payload), err: io.ErrUnexpectedEOF}
	r := newTestRRC(body, -1, func(offset int64) (io.ReadCloser, error) {
		if offset != int64(len(payload)) {
			t.Fatalf("reconnect at unexpected offset %d", offset)
		}
		return nil, errUpstreamEOF
	})
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("expected clean EOF, got %v", err)
	}
	if string(got) != payload {
		t.Fatalf("payload mismatch: %q", got)
	}
}

func TestRetryMidStreamResumes(t *testing.T) {
	full := "0123456789"
	body := &dirtyReader{data: strings.NewReader(full[:4]), err: io.ErrUnexpectedEOF}
	r := newTestRRC(body, int64(len(full)), func(offset int64) (io.ReadCloser, error) {
		if offset != 4 {
			t.Fatalf("reconnect at unexpected offset %d", offset)
		}
		return io.NopCloser(strings.NewReader(full[4:])), nil
	})
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("expected clean read, got %v", err)
	}
	if string(got) != full {
		t.Fatalf("payload mismatch: %q", got)
	}
}

func TestRetryReconnectFailureReturnsOriginalError(t *testing.T) {
	body := &dirtyReader{data: strings.NewReader("0123"), err: io.ErrUnexpectedEOF}
	r := newTestRRC(body, 10, func(offset int64) (io.ReadCloser, error) {
		return nil, io.ErrClosedPipe
	})
	_, err := io.ReadAll(r)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("expected original error, got %v", err)
	}
}
