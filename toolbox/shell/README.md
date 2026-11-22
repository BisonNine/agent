# shell — shell commands and background jobs

`go.bison9.com/agent/toolbox/shell` gives an agent two tools, published under
the namespace `shell`:

| Tool | Purpose |
| --- | --- |
| `shell_exec` | run a command in the foreground, or start it as a background job |
| `shell_ctrl` | list, wait on, send input to, or stop the agent's jobs |

Underneath, `toolbox/shell/proc` is a process manager that knows nothing about
agents: it runs `sh -c` jobs in their own process groups, writes their output
to files, and tracks them. `proc/proctest` has a fake clock for tests.

## Behaviour

- **Foreground.** `shell_exec` waits up to `timeout_seconds` (default 60, at
  most 600). Output longer than 10,000 bytes comes back as its head and its
  tail, and the result names the log file that holds all of it. A command
  still running at its timeout is **not killed**: it is kept as a job, and
  the result gives its id and log file.
- **Background.** `background: true` returns at once with the job id and log
  file. With `notify` (the default) the agent is told once when the job
  ends.
- **Output** goes to `<id>.log` as it is produced, stdout and stderr
  interleaved. Past the per-job limit the file keeps its head and its tail
  and records how many bytes were dropped; the job keeps running. There is
  no read action: the agent reads the log with the file tools.
- **Working directory.** After a foreground command, the directory it ended
  in is the default for the agent's next command in the same conversation.
  Nothing else carries over.
- **Ownership.** A job belongs to the owner that started it (the host decides
  who that is; a clone should act as its source). Another owner's job id is
  unknown.
- **Stopping** terminates the whole process group, then kills it after a
  grace period (5 s). Jobs over `MaxJobTime` are stopped the same way.
- **Restart.** A job recorded as running when the manager starts was lost
  with the old process: it is marked `lost`, announced once, and never
  relaunched.
- **Shutdown.** `Close` stops every job.
- **Without shell_ctrl or unattended.** When `CtrlAvailable` says the caller
  has no `shell_ctrl`, or `Unattended` says nobody will collect a job, a
  foreground timeout stops the command and `background` is refused;
  unattended callers are also refused `shell_ctrl`.
- **Platforms.** Jobs need Unix process groups. Elsewhere `shell_exec` runs
  the command (PowerShell on Windows) and waits, killing it at the timeout;
  `background` and `shell_ctrl` are refused.

## Limits (`proc.Limits`)

| Field | Default | Meaning |
| --- | --- | --- |
| `MaxJobs` | 8 | running jobs per owner; a start beyond it is refused, naming the running ones |
| `MaxJobTime` | 24 h | a job is stopped after this long |
| `MaxOutputBytes` | 10 MiB | per job; beyond it the middle of the log is dropped |
| `LogRetention` | 7 days | ended jobs' logs and records are removed after this |

Foreground commands are jobs while they run, so they count towards
`MaxJobs`. A foreground command whose whole output was returned leaves no
log behind.

## Mounting it in a host

```go
tools, err := shell.New(shell.Config{
    Jobs: proc.Options{
        Dir:    filepath.Join(workspace, "state", "shell"),
        Limits: proc.Limits{MaxJobs: 8},
        Modes:  fsmode.Modes{Dir: 0o700, File: 0o600},
    },
    Timeout:       60 * time.Second,
    MaxTimeout:    600 * time.Second,
    Guard:         guardCommand,                 // "" allows; otherwise the refusal text
    Env:           func(*toolspec.ToolCall) []string { return childEnv },
    Workspace:     func(*toolspec.ToolCall) string { return workspace },
    Owner:         func(c *toolspec.ToolCall) string { return sourceAgent(c.AgentID) },
    CtrlAvailable: func(c *toolspec.ToolCall) bool { return granted(c.AgentID, "shell_ctrl") },
    Unattended:    isUnattended,
    Notice:        deliverToConversation,        // once per job that asked to be told
    Audit:         recordAudit,                  // start, input (length only), stop
    Logger:        slog.Default(),
})
// Register tools (a toolspec.ToolProvider and HostMeta) under "shell".
// "/cancel all" for an agent: tools.StopAll(owner); count: len(tools.Running(owner)).
// Shutdown: tools.Close(ctx).
```

A guard-refused or not-allowed call returns a result whose `Err` wraps
`shell.ErrRefused`, so the host can log it as a refusal rather than a
failure.
