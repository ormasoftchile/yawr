// toolTokenStore.ts — Extension-host-only tool invocation token store.
//
// The toolInvocationToken is obtainable ONLY from a ChatRequestHandler
// (vscode.chat.createChatParticipant). This module holds the captured token
// in extension-host memory only and exposes it to the MCP bridge.
//
// Security invariants:
//   - Never serialized, logged, or forwarded to Yawr Core.
//   - Never placed in env vars, HTTP response bodies, error text, or traces.
//   - Cleared on extension deactivation.
//   - Cleared when VS Code rejects the token (stale session detection).
//   - Never reused across VS Code sessions.
//
// This module deliberately imports NOTHING from `vscode` so it can be
// exercised by plain `node --test` unit tests without a host context.

let _token: unknown = undefined;

/** Store the token captured from a ChatRequestHandler arm-mcp command. */
export function setToolToken(token: unknown): void {
  _token = token;
}

/** Return the currently stored token, or undefined when unarmed. */
export function getToolToken(): unknown {
  return _token;
}

/** Clear the stored token. Call on extension deactivation or token rejection. */
export function clearToolToken(): void {
  _token = undefined;
}

/** True when a token has been captured and the bridge is armed. */
export function isArmed(): boolean {
  return _token !== undefined;
}

/**
 * Reset the store to the unarmed baseline.
 * Must only be called from test code — never from production paths.
 */
export function _resetForTest(): void {
  _token = undefined;
}
