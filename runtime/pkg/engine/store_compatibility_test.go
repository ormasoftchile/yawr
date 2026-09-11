package engine

import "context"

type legacyRunStore struct{}

func (*legacyRunStore) SaveState(context.Context, RunState) error           { return nil }
func (*legacyRunStore) LoadState(context.Context, string) (RunState, error) { return RunState{}, nil }
func (*legacyRunStore) WriteTrace(context.Context, string, Event) error     { return nil }
func (*legacyRunStore) Close() error                                        { return nil }

func ExampleRunStore_sourceCompatibility() {
	var store RunStore = &legacyRunStore{}
	_ = EngineConfig{Store: store}
	_ = RunOptions{Store: store}
}
