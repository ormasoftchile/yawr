package extension

// ExtensionState is the lifecycle state of an extension.
type ExtensionState int

const (
	StateUnloaded ExtensionState = iota
	StateDiscovered
	StateStarting
	StateHandshaking
	StateLoaded
	StateFailed
	StateShutdown
)
