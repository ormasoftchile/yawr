package trace

// TraceWriter appends events to the append-only JSONL trace file.
// Implementations MUST be safe for concurrent use by multiple goroutines.
type TraceWriter interface {
	// Append serialises event and appends it as a single JSON line.
	// The write MUST be atomic with respect to concurrent callers.
	Append(event TraceEvent) error

	// Close flushes any buffered data and releases the underlying file handle.
	Close() error
}
