// chatParticipantGate.ts — pure arm-command gate logic.
//
// The gate check must live in a pure module (no vscode import) so it can be
// unit-tested with plain node --test and so that mutation 2 (capture on ANY
// command) produces a test failure rather than silently passing.
//
// Extension.ts uses isArmCommand(request.command) instead of inlining the
// comparison, making the gate testable independently of the vscode binding.

/**
 * Returns true only when command is exactly "arm-mcp".
 *
 * This gate is intentionally narrow. Accessing request.toolInvocationToken
 * for any other command must not populate the token store.
 */
export function isArmCommand(command: string | undefined): boolean {
  return command === 'arm-mcp';
}
