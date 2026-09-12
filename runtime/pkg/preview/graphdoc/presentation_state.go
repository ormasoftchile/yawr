package graphdoc

type PresentationState struct {
	RunID              string                   `json:"run_id"`
	PlanSnapshotDigest string                   `json:"plan_snapshot_digest"`
	CheckpointSequence uint64                   `json:"checkpoint_sequence"`
	Occurrences        []PresentationOccurrence `json:"occurrences"`
}
type PresentationOccurrence struct {
	Identity          map[string]any    `json:"identity"`
	Details           *StepDetails      `json:"details"`
	Output            map[string]any    `json:"output"`
	OutputValueStatus map[string]string `json:"output_value_status"`
}
