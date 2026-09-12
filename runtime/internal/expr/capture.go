package expr

// ResolveCapturePath resolves a capture source against a step output map using
// the native GCP capture engine. The bool reports whether the source resolved.
func (e *TemplateEvaluator) ResolveCapturePath(source string, output map[string]any) (any, bool, error) {
	selectedEngine(Operation{Kind: ResolveCapturePath, Source: source})
	return resolveCaptureNative(source, captureInput{
		Stdout:   output["stdout"],
		Stderr:   output["stderr"],
		ExitCode: output["exit_code"],
		Output:   output,
	})
}
