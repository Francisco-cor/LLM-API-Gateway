package provider

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

// sharedTransport is tuned for high throughput gateway -> upstream LLM providers.
// MaxIdleConns and KeepAlive reduce TLS handshake latency at 10k rps.
var sharedTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   20,
	MaxConnsPerHost:       100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   5 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ForceAttemptHTTP2:     true,
}

// newHTTPClient returns an *http.Client that reuses sharedTransport but respects
// per-provider timeout (config.providers.<name>.timeout).
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: sharedTransport,
	}
}

// buffer pool for JSON encoding to avoid repeated allocations at high RPS.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// marshalJSON encodes v to JSON using a pooled buffer (Fase 10: json pooled buffers).
// It is semantically equivalent to json.Marshal but reuses buffers.
func marshalJSON(v any) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	// json.Encoder adds trailing newline; trim it to match json.Marshal output
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// pooledBuffer helper for response encoding (exported for proxy use).
func GetPooledBuffer() *bytes.Buffer {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

func PutPooledBuffer(buf *bytes.Buffer) {
	bufPool.Put(buf)
}
