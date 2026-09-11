package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"os"
)

func runPresentation(args []string) int {
	if len(args) == 2 && args[0] == "capabilities" && args[1] == "--v3" {
		if json.NewEncoder(os.Stdout).Encode(presentation.TypedResultsCapabilities()) != nil {
			return exitFailure
		}
		return exitSuccess
	}
	if len(args) > 1 && args[0] == "capabilities" {
		fmt.Fprintln(os.Stderr, "presentation: unsupported-version")
		return exitValidation
	}
	if len(args) > 0 && args[0] == "expressions" {
		return runExpressionPresentation(args[1:])
	}
	if len(args) == 1 && args[0] == "capabilities" {
		fmt.Fprintln(os.Stdout, `{"schema_version":"yawr.presentation-capabilities/v1","resolver_version":"yawr.core-binding/v1","execution_plan_read":["execution-plan/v3"],"execution_plan_write":["execution-plan/v3"]}`)
		return exitSuccess
	}

	if len(args) != 2 || args[0] != "resolve" || args[1] != "--stdio" {
		fmt.Fprintln(os.Stderr, "usage: yawr presentation resolve --stdio | capabilities")
		return exitValidation
	}
	req, err := presentation.DecodeRequest(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "presentation: invalid-request")
		return exitValidation
	}
	reply := presentation.Resolve(req)
	data, err := json.Marshal(reply)
	if err != nil || len(data) > presentation.MaxBytes {
		reply.Status, reply.Reason = "unavailable", "limit-exceeded"
		reply.Bindings = []presentation.Binding{}
		reply.Regions = []presentation.Region{}
		reply.Dependencies = []presentation.Dependency{}
		data, err = json.Marshal(reply)
		if err != nil || len(data) > presentation.MaxBytes {
			fmt.Fprintln(os.Stderr, "presentation: limit-exceeded")
			return exitValidation
		}
	}
	if _, err = os.Stdout.Write(append(data, '\n')); err != nil {
		return exitFailure
	}
	return exitSuccess
}

func runExpressionPresentation(args []string) int {
	if len(args) == 1 && args[0] == "capabilities" {
		if json.NewEncoder(os.Stdout).Encode(presentation.ExpressionsCapabilities()) != nil {
			return exitFailure
		}
		return exitSuccess
	}
	if len(args) != 2 || args[0] != "resolve" || args[1] != "--stdio" {
		fmt.Fprintln(os.Stderr, "usage: yawr presentation expressions resolve --stdio | capabilities")
		return exitValidation
	}
	req, err := presentation.DecodeExpressionRequest(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "presentation expressions:", err)
		return exitValidation
	}
	reply := presentation.ResolveExpressions(context.Background(), req)
	data, err := json.Marshal(reply)
	if err != nil || len(data) > presentation.MaxBytes {
		fmt.Fprintln(os.Stderr, "presentation expressions: limit-exceeded")
		return exitValidation
	}
	if _, err = os.Stdout.Write(append(data, '\n')); err != nil {
		return exitFailure
	}
	return exitSuccess
}
