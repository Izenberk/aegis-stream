package frame

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Write sends a length-prefixed frame to w.
// Takes io.Writer (not net.Conn) so it works with any writer: TCP, files, buffers.
func Write(w io.Writer, data []byte) error {
	// Build the 4-byte Big Endian header (see NOTES.md: TCP Length-Prefix Framing)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))

	// Send header first, then payload — this is the framing protocol
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("frame: write header: %w", err) // %w wraps the error for errors.Is/As
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("frame: write payload: %w", err)
	}
	return nil
}

// Read reads a single length-prefixed frame from r.
// Uses io.ReadFull to guarantee we get exact byte counts (see NOTES.md: Why io.ReadFull)
func Read(r io.Reader) ([]byte, error) {
	// Step 1: Read exactly 4 bytes for the length header
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("frame: read header: %w", err)
	}

	// Step 2: Decode header to know how many payload bytes to expect
	size := binary.BigEndian.Uint32(header)

	// Step 3: Read exactly 'size' bytes for the payload
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("frame: read payload: %w", err)
	}
	return data, nil
}
