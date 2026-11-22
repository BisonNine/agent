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
- **Shutdown.** `Close` stops every job and delivers no notice, since the
  host is stopping. A job it stopped with `notify` on is announced once by
  the next `New` on the same directory (`StopReason` "shutdown": "Shell job
  … was stopped when the service restarted, and was not started again.").
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

**Changing them at run time.** `tools.Update(shell.Settings{...})` (or
`Manager().SetLimits`) is checked like `New`: a negative value is refused
and nothing changes; zero is the default. It applies to calls that start
after it returns:

| Setting | Takes effect |
| --- | --- |
| `Timeout`, `MaxTimeout` | for later calls; a foreground command already waiting keeps its timeout |
| `MaxJobs` | for later starts; lowering it stops nothing, starts are refused until below it |
| `MaxJobTime` | for running jobs too, from their start: one already older is stopped at once ("time limit") |
| `MaxOutputBytes` | for output written from now on; a head already kept is not shrunk |
| `LogRetention` | at the next sweep (the next start) |

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
    Route:         func(c *toolspec.ToolCall) any { return conversationOf(c) },
    Logger:        slog.Default(),
})
// Register tools (a toolspec.ToolProvider and HostMeta) under "shell".
// "/cancel all" for an agent: tools.StopAll(owner); count: len(tools.Running(owner)).
// Configuration reload: tools.Update(shell.Settings{Timeout: ..., MaxTimeout: ..., Limits: ...}).
// Shutdown: tools.Close(ctx).
```

### Config

| Field | Zero value | Meaning |
| --- | --- | --- |
| `Jobs` | required `Dir` | the process manager's options (`proc.Options`); its `OnEnd` runs after the tools' own |
| `Timeout`, `MaxTimeout` | 60 s, 600 s | default and largest foreground timeout; `Update` changes them |
| `Guard` | allows all | returns why a command is refused, or "" |
| `Env` | inherits | the child environment for a call |
| `Workspace` | process directory | default working directory, and the base of a relative one |
| `Owner` | `call.AgentID` | who owns the jobs a call starts |
| `CtrlAvailable` | available | whether the caller has `shell_ctrl` |
| `Unattended` | attended | whether nobody will collect a job |
| `Notice` | no notices | delivers an end-of-job notice, once per job |
| `Audit` | not audited | records starts, input and stops |
| `Route` | nil route | the host's routing value for a job, see below |
| `Logger` | discarded | diagnostics |

### What the host needs to do

- **Route notices with `Route`, not from the audit.** `Route` is called once
  for every job about to start (foreground commands included: one kept at
  its timeout is announced), before the process starts, and its value comes
  back as `Notice.Route` and `AuditEvent.Route`. A notice is never delivered
  before `Route` has returned for its job, so a job that ends at once is
  still routed. `AuditStart` comes after `Start` returns and may come after
  the notice.
- **Route jobs announced after a restart by `Notice.Owner`.** The route is
  kept in memory only, so a job found `lost`, or one stopped by the last
  shutdown, has a nil `Route`. These notices are delivered during `New`.
- **Do not expect notices from `Close`.** Jobs it stops are announced by
  the next `New` instead.
- **Apply a configuration reload with `Update`.** Tools built before the
  reload then follow it; there is no need to rebuild them.

A guard-refused or not-allowed call returns a result whose `Err` wraps
`shell.ErrRefused`, so the host can log it as a refusal rather than a
failure.
