package sink

import "aegis-stream/pb"

// Sink is the interface that all event destinations must implement.
// Workers call Write() for every processed event. The pipeline calls
// Close() on shutdown to flush buffers and release resources.
type Sink interface {
	Write(event *pb.Event) error
	Close() error
}
