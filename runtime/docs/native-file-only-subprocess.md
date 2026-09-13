# Native file-only subprocess transport

`native-file-only` is a distinct Windows AMD64 transport. It is not an alias
for `native` or `mcp`, and it never falls back to either transport.

```yaml
transport:
  mode: native-file-only
  command: bin\reader.exe
  sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  inputs:
    - data\input.json
```

`command` and every `inputs` entry must be an explicit package-relative path
to a regular file. Absolute paths, traversal, PATH lookup, directories,
symlinks, junctions, and other reparse points reject. `sha256` is mandatory,
lower-case, and verified over the bytes copied into the sandbox before those
same staged bytes execute.

The transport contract is closed: `command`, `sha256`, and `inputs` are the
only transport fields accepted by this mode. `transport.args` is rejected;
invocation arguments belong on each action's `argv`. `env`, `url`, `auth`,
and `vscode_tool` are also rejected rather than ignored.

The runtime creates an ephemeral AppContainer with no capabilities, grants it
read/execute access only to staged files, and grants write access only to the
private `scratch` directory. A Job Object with an active-process limit of one
denies child creation and kills the complete process tree on cancellation,
overflow, or cleanup. Stdin is closed. Stdout and stderr are each limited to
1 MiB, and execution is limited to 30 seconds (or the earlier caller
deadline). The sandbox and AppContainer profile are removed on all paths.

`transport.env` is forbidden. The child receives only `SystemRoot`, `WINDIR`,
`SystemDrive`, `ProgramData`, `ALLUSERSPROFILE`, `USERPROFILE`, `APPDATA`, and
`LOCALAPPDATA` for Windows process initialization, plus `TEMP` and `TMP`
redirected to private scratch. AppContainer ACL enforcement prevents access to
the referenced host profile/config paths; tests validate that a parent
sentinel is absent and arbitrary host sentinel reads and writes fail.

Successful bounded stdout is parsed by the existing action `result` contract;
typed Results remain authoritative. Unsupported platforms and unavailable
AppContainer backends reject before invocation and do not advertise
`yawr.file-only-subprocess/v1`.
