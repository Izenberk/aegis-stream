package sink

import (
	"log/slog"

	"aegis-stream/pb"
)

// StdoutSink writes events to stdout via structured logging.
// This is the default sink — same behavior as before the Sink interface existed.
type StdoutSink struct{}

func NewStdout() *StdoutSink {
	return &StdoutSink{}
}

func (s *StdoutSink) Write(event *pb.Event) error {
	switch payload := event.Payload.(type) {
	case *pb.Event_Trade:
		slog.Info("routed trade",
			"symbol", payload.Trade.Symbol,
			"price", payload.Trade.Price,
			"volume", payload.Trade.Volume,
		)
	case *pb.Event_Log:
		slog.Info("routed log",
			"level", payload.Log.Level,
			"message", payload.Log.Message,
			"service", payload.Log.ServiceName,
		)
	}
	return nil
}

func (s *StdoutSink) Close() error {
	return nil
}
