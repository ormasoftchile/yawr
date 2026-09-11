package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
)

func runAuthoring(args []string) int {
	if len(args) == 2 && args[0] == "capabilities" && args[1] == "--v3" {
		if json.NewEncoder(os.Stdout).Encode(presentation.AuthoringTypedCapabilities()) != nil {
			return exitFailure
		}
		return exitSuccess
	}
	if len(args) == 2 && args[0] == "capabilities" && args[1] == "--v2" {
		if json.NewEncoder(os.Stdout).Encode(presentation.AuthoringIncludeCapabilities()) != nil {
			return exitFailure
		}
		return exitSuccess
	}
	if len(args) == 1 && args[0] == "capabilities" {
		if json.NewEncoder(os.Stdout).Encode(presentation.AuthoringCapabilities()) != nil {
			return exitFailure
		}
		return exitSuccess
	}
	if len(args) > 0 && args[0] == "capabilities" {
		fmt.Fprintln(os.Stderr, "authoring: unsupported-version")
		return exitValidation
	}
	if len(args) != 2 || args[1] != "--stdio" || (args[0] != "complete" && args[0] != "signature" && args[0] != "required-arguments") {
		fmt.Fprintln(os.Stderr, "authoring: invalid-request")
		return exitValidation
	}
	req, err := presentation.DecodeAuthoringRequest(os.Stdin, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "authoring: invalid-request")
		return exitValidation
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply := presentation.ResolveAuthoring(ctx, req)
	data, err := json.Marshal(reply)
	if err != nil || len(data) > presentation.MaxBytes {
		fmt.Fprintln(os.Stderr, "authoring: limit-exceeded")
		return exitValidation
	}
	if _, err = os.Stdout.Write(append(data, '\n')); err != nil {
		return exitFailure
	}
	return exitSuccess
}
