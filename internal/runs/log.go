package runs

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// gzipLog compresses the raw EventLog JSON with the standard library at
// best-speed. Ingestion is on the request hot path and logs are small (≤ 2 MB),
// so speed matters more than ratio; the worker never re-reads for compression.
func gzipLog(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, fmt.Errorf("new gzip writer: %w", err)
	}
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// gunzipLog reverses gzipLog, returning the original EventLog JSON bytes
// byte-for-byte. Used by the ?log=1 detail flag (the frontend's replay feature).
func gunzipLog(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("new gzip reader: %w", err)
	}
	defer func() { _ = zr.Close() }()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("gzip read: %w", err)
	}
	return raw, nil
}
