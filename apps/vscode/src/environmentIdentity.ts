export function environmentValue(
  environment: NodeJS.ProcessEnv,
  suffix: string,
): string | undefined {
  return environment[`YAWR_${suffix}`];
}

export function runtimeEnvironment(suffix: string, value: string): Record<string, string> {
  return { [`YAWR_${suffix}`]: value };
}
