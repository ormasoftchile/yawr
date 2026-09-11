package presentation

func TypedResultsCapabilities() any {
	return struct {
		SchemaVersion      string   `json:"schema_version"`
		ResolverVersion    string   `json:"resolver_version"`
		ExecutionPlanRead  []string `json:"execution_plan_read"`
		ExecutionPlanWrite []string `json:"execution_plan_write"`
		Capabilities       []string `json:"capabilities"`
		GraphRead          []string `json:"graph_read"`
		AuthoringRequest   string   `json:"authoring_request"`
		ExpressionRequest  string   `json:"expression_request"`
		StdioVersion       string   `json:"stdio_version"`
		StdioFrameBytes    int      `json:"stdio_frame_bytes"`
		ResultsChunkBytes  int      `json:"results_chunk_bytes"`
		ResultsMaxBytes    int      `json:"results_max_bytes"`
	}{"presentation-capabilities/v3", "core-binding/v3",
		[]string{"execution-plan/v3"},
		[]string{"execution-plan/v3"},
		[]string{"yawr.typed-results/v1", "yawr.run-results-chunks/v1", "yawr.run-get-results/v1"},
		[]string{"1", "3"}, AuthoringRequestVersion, ExpressionSchemaVersion,
		"yawr.stdio/v1", 1 << 20, 64 << 10, 256 << 20}
}

func AuthoringTypedCapabilities() any {
	return struct {
		SchemaVersion     string           `json:"schema_version"`
		ResolverVersion   string           `json:"resolver_version"`
		GrammarVersion    string           `json:"grammar_version"`
		Operations        []string         `json:"operations"`
		DiscoveryScope    string           `json:"discovery_scope"`
		MaxBytes          int              `json:"max_bytes"`
		MaxOverlays       int              `json:"max_overlays"`
		MaxItems          int              `json:"max_items"`
		MaxValueCodeUnits int              `json:"max_value_code_units"`
		MaxDepth          int              `json:"max_depth"`
		Capabilities      []string         `json:"capabilities"`
		CaptureRoots      []string         `json:"capture_roots"`
		TypedOperations   []TypedOperation `json:"typed_operations"`
	}{"authoring-capabilities/v3", "core-authoring/v3", ExpressionGrammarVersion,
		[]string{"complete", "signature", "required-arguments"}, "explicit-local-catalog", MaxBytes, 128, MaxEntries, 32768, 128,
		[]string{"yawr.typed-results/v1"}, []string{"outputs"}, []TypedOperation{
			{Kind: "assign", Role: "technical", Terminal: false},
			{Kind: "results", Title: "Results", Role: "operator", Terminal: true},
		}}
}

type TypedOperation struct {
	Kind     string `json:"kind"`
	Title    string `json:"title,omitempty"`
	Role     string `json:"role"`
	Terminal bool   `json:"terminal"`
}
