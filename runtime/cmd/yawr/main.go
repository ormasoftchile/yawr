package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	// Handle --version flag before command dispatch.
	if os.Args[1] == "--version" {
		os.Exit(runVersion(nil))
	}
	switch os.Args[1] {
	case "plan":
		os.Exit(runPlan(os.Args[2:]))
	case "run":
		os.Exit(runRun(os.Args[2:]))
	case "dry-run":
		os.Exit(runDryRun(os.Args[2:]))
	case "compile":
		os.Exit(runCompile(os.Args[2:]))
	case "ls":
		os.Exit(runLs(os.Args[2:]))
	case "gc":
		os.Exit(runGc(os.Args[2:]))
	case "kit":
		os.Exit(runKit(os.Args[2:]))
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "session":
		os.Exit(runSession(os.Args[2:]))
	case "preview":
		os.Exit(runPreview(os.Args[2:]))
	case "presentation":
		os.Exit(runPresentation(os.Args[2:]))
	case "authoring":
		os.Exit(runAuthoring(os.Args[2:]))
	case "version":
		os.Exit(runVersion(os.Args[2:]))
	case "dev":
		runDev(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runDev(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: yawr dev <subcommand>")
		os.Exit(1)
	}
	switch args[0] {
	case "spec-coverage":
		fmt.Println("spec-coverage: not yet implemented")
	default:
		fmt.Fprintf(os.Stderr, "unknown dev subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "yawr — Yet Another Workflow Runtime")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Usage: yawr <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  plan      Statically analyse a runbook for a given runtime profile")
	fmt.Fprintln(os.Stderr, "  run       Execute a runbook")
	fmt.Fprintln(os.Stderr, "  dry-run   Validate a runbook without side effects")
	fmt.Fprintln(os.Stderr, "  compile   Compile and validate platform kit tools")
	fmt.Fprintln(os.Stderr, "  serve     Start the JSON-RPC HTTP server")
	fmt.Fprintln(os.Stderr, "  session   Attach to a durable investigation session")
	fmt.Fprintln(os.Stderr, "  preview   Render a runbook as prose, Mermaid, or React Flow JSON")
	fmt.Fprintln(os.Stderr, "  ls        List past runs")
	fmt.Fprintln(os.Stderr, "  gc        Clean up old runs")
	fmt.Fprintln(os.Stderr, "  kit       Manage yawr kits")
	fmt.Fprintln(os.Stderr, "  version   Print version information")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Run 'yawr <command> --help' for command-specific flags.")
}
