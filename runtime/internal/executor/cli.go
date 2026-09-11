package executor

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/capture"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/expr"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// ScriptChecker is an optional interface that a GovernancePolicy may implement
// to apply deny/allow checks against a complete run: script string rather than
// argv[0] alone. CLIExecutor uses this when spec.Run is set so that patterns
// like "rm -rf *" correctly bind shell scripts, not just the shell binary.
type ScriptChecker interface {
	CheckScript(script string) (allowed bool, matchedRule string)
}

// CLIExecutor executes CLI steps via the Platform abstraction.
type CLIExecutor struct {
	platform  platform.Platform
	evaluator expr.Evaluator
}

// NewCLIExecutor constructs a CLIExecutor.
func NewCLIExecutor(p platform.Platform, eval expr.Evaluator) *CLIExecutor {
	return &CLIExecutor{platform: p, evaluator: eval}
}

// Execute runs a CLI step.
func (e *CLIExecutor) Execute(ctx context.Context, step engine.ResolvedStep, vars map[string]any) (*engine.StepResult, error) {
	spec, ok := step.Spec.(*schema.CLISpec)
	if !ok || spec == nil {
		return nil, fmt.Errorf("cli executor: invalid spec for step %s", step.ID)
	}

	resolvedVars := vars
	shell := spec.Shell
	if shell == "" && e.platform != nil {
		shell = e.platform.DefaultShell()
	}

	command := spec.Command
	args := spec.Args
	stdin := spec.Stdin
	workdir := spec.Workdir
	if e.evaluator != nil {
		var err error
		if command != "" {
			command, err = resolveTemplate(e.evaluator, command, resolvedVars)
			if err != nil {
				return nil, err
			}
		}
		args, err = resolveStringSlice(e.evaluator, args, resolvedVars)
		if err != nil {
			return nil, err
		}
		if stdin != "" {
			stdin, err = resolveTemplate(e.evaluator, stdin, resolvedVars)
			if err != nil {
				return nil, err
			}
		}
		if workdir != "" {
			workdir, err = resolveTemplate(e.evaluator, workdir, resolvedVars)
			if err != nil {
				return nil, err
			}
		}
		if shell != "" {
			shell, err = resolveTemplate(e.evaluator, shell, resolvedVars)
			if err != nil {
				return nil, err
			}
		}
	}

	var resolvedScript string
	if spec.Run != nil {
		script, err := resolveRunScript(spec.Run, shell)
		if err != nil {
			return nil, err
		}
		if e.evaluator != nil {
			script, err = resolveTemplate(e.evaluator, script, resolvedVars)
			if err != nil {
				return nil, err
			}
		}
		if shell == "" {
			return nil, fmt.Errorf("cli executor: shell is required for run script")
		}
		resolvedScript = script
		command = shell
		args = buildShellArgs(shell, script)
	}

	if command == "" {
		return nil, fmt.Errorf("cli executor: command is required")
	}

	// Enforce any composed governance policy inherited from a dynamic include
	// parent. For command: steps, CheckCommand checks argv[0]. For run: steps,
	// CheckScript checks the full script content so patterns like "rm -rf *"
	// bind correctly (path.Match's * does not cross /, so we need prefix
	// semantics for script content — see ScriptChecker).
	if pol := govPolicyFromCtx(ctx); pol != nil {
		var govAllowed bool
		var govMatchedRule string
		if resolvedScript != "" {
			if sc, ok := pol.(ScriptChecker); ok {
				govAllowed, govMatchedRule = sc.CheckScript(resolvedScript)
			} else {
				// Fallback for policies that don't implement ScriptChecker:
				// extract argv[0] from the script.
				if fields := strings.Fields(resolvedScript); len(fields) > 0 {
					base := fields[0]
					if idx := strings.LastIndexAny(base, "/\\"); idx >= 0 {
						base = base[idx+1:]
					}
					govAllowed, govMatchedRule = pol.CheckCommand(base)
				} else {
					govAllowed = true
				}
			}
		} else {
			govAllowed, govMatchedRule = pol.CheckCommand(command)
		}
		if !govAllowed {
			reason := "governance policy"
			if govMatchedRule != "" {
				reason = govMatchedRule
			}
			return nil, fmt.Errorf("cli executor: step %s: command denied by composed governance [GOVERNANCE-001]: %s", step.ID, reason)
		}
	}

	resolvedEnv := map[string]string{}
	for k, v := range spec.Env {
		if e.evaluator == nil {
			resolvedEnv[k] = v
			continue
		}
		resolved, err := resolveTemplate(e.evaluator, v, resolvedVars)
		if err != nil {
			return nil, err
		}
		resolvedEnv[k] = resolved
	}
	if len(spec.Env) == 0 {
		resolvedEnv = nil
	}

	if e.platform == nil {
		return nil, fmt.Errorf("cli executor: platform is required")
	}
	dispatch, err := engine.PrepareExternalDispatch(ctx, engine.DispatchRequest{
		Classification: "unspecified", EndpointIdentity: "local-process",
		RenderedRequest: map[string]any{
			"command": command, "args": args, "env": resolvedEnv,
			"workdir": workdir, "stdin": stdin, "shell": shell,
		},
	})
	if err != nil {
		return nil, err
	}
	ctx = engine.WithPreparedDispatch(ctx, dispatch)
	engine.RecordRouteTestExternalDispatch(ctx)
	result, err := e.platform.Exec(ctx, platform.ExecRequest{
		Command: command,
		Args:    args,
		Env:     resolvedEnv,
		Workdir: workdir,
		Stdin:   stdin,
		Shell:   shell,
	})
	if err != nil {
		return nil, err
	}

	stepResult := newResult(step, engine.StepStatusCompleted)
	stepResult.Output["stdout"] = result.Stdout
	stepResult.Output["stderr"] = result.Stderr
	stepResult.Output["exit_code"] = result.ExitCode
	if result.ExitCode != 0 {
		stepResult.Status = engine.StepStatusFailed
		stepResult.Outcome = outcomeForStatus(stepResult.Status)
	}

	if len(step.Capture) > 0 {
		captures, err := capture.New(vars, nil, nil).CaptureStep(nil, step, stepResult)
		if err != nil {
			return nil, fmt.Errorf("cli executor: %w", err)
		}
		for name, val := range capture.ToAnyMap(captures) {
			stepResult.Vars[name] = val
		}
	}

	return stepResult, nil
}

func resolveRunScript(run any, shell string) (string, error) {
	switch v := run.(type) {
	case string:
		return v, nil
	case map[string]string:
		return pickRunScript(v, shell), nil
	case map[string]any:
		converted := make(map[string]string)
		for k, raw := range v {
			if s, ok := raw.(string); ok {
				converted[k] = s
			}
		}
		return pickRunScript(converted, shell), nil
	default:
		return "", fmt.Errorf("cli executor: unsupported run type %T", run)
	}
}

func pickRunScript(scripts map[string]string, shell string) string {
	if len(scripts) == 0 {
		return ""
	}
	if shell != "" {
		lowerShell := strings.ToLower(filepath.Base(shell))
		for k, script := range scripts {
			if strings.Contains(lowerShell, strings.ToLower(k)) {
				return script
			}
		}
	}
	keys := make([]string, 0, len(scripts))
	for k := range scripts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return scripts[keys[0]]
}

func buildShellArgs(shell string, script string) []string {
	lower := strings.ToLower(shell)
	flag := "-c"
	if strings.Contains(lower, "cmd.exe") || strings.HasSuffix(lower, "cmd") {
		flag = "/C"
	} else if strings.Contains(lower, "powershell") || strings.Contains(lower, "pwsh") {
		flag = "-Command"
	}
	return []string{flag, script}
}
