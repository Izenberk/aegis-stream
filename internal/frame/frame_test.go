package frame_test

import (
	"bytes"
	"testing"

	"aegis-stream/internal/frame"
)

// TestRoundTrip: write a frame then read it back — bytes should match exactly.
// bytes.Buffer acts as a fake network connection (implements both io.Writer and io.Reader).
func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello aegis")

	if err := frame.Write(&buf, payload); err != nil {
		t.Fatalf("Write failed: %v", err) // Fatalf = log + stop this test immediately
	}

	got, err := frame.Read(&buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload) // Errorf = log but keep running
	}
}

// TestMultipleFrames: framing must preserve message boundaries.
// Three writes followed by three reads should return messages in order.
func TestMultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	messages := [][]byte{
		[]byte("trade:BTC-USD"),
		[]byte("log:high latency"),
		[]byte("trade:ETH-USD"),
	}

	for _, msg := range messages {
		if err := frame.Write(&buf, msg); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	for i, want := range messages {
		got, err := frame.Read(&buf)
		if err != nil {
			t.Fatalf("Read frame %d failed: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("frame %d: got %q, want %q", i, got, want)
		}
	}
}

// TestEmptyPayload: edge case — a zero-length message should still work.
// The 4-byte header will contain 0x00000000, and Read returns an empty slice.
func TestEmptyPayload(t *testing.T) {
	var buf bytes.Buffer

	if err := frame.Write(&buf, []byte{}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	got, err := frame.Read(&buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(got))
	}
}

// TestLargePayload: test with 8KB — exceeds the server's 4KB sync.Pool buffer.
// Proves framing works regardless of payload size.
func TestLargePayload(t *testing.T) {
	var buf bytes.Buffer
	payload := make([]byte, 8192) // 8KB
	for i := range payload {
		payload[i] = byte(i % 256) // fill with a pattern so we can detect corruption
	}

	if err := frame.Write(&buf, payload); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	got, err := frame.Read(&buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("large payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}
