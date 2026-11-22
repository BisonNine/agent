# ident

`go.bison9.com/agent/ident` is the agent id rule, in one place for a host's
configuration, its forum and classifiers, and the agent loop. It imports
only the standard library.

- An agent id is 1 to 64 characters (`MaxIDLength`) of ASCII letters (either
  case), digits, `-` and `_`, and starts with a letter or digit. `ValidID`
  checks it.
- Ids are case-insensitive. An agent's identity is `NormalizeID(id)`: a
  valid id in lower case. A host keeps an id as it was written and uses the
  identity wherever the id names something (a folder, a session, a record).
- `NormalizeID` repairs other input first (`RepairID`) and returns `""` when
  nothing of it survives. A host that routes such input to a default agent
  substitutes that agent's id itself.
- `SameID(a, b)`: both have an identity and the identities are equal. Every
  comparison of agent ids goes through it.
- `RepairID(id)` is the valid id to suggest when an id is refused, keeping
  its case: each run of other characters becomes one `-`, leading and
  trailing `-` and `_` go, and the result is cut to 64 characters.

Only ASCII letters change case: Unicode case mapping differs between
languages, so a web front end can implement the same rule exactly.

| Input | `ValidID` | `NormalizeID` |
| --- | --- | --- |
| `Alice` | yes | `alice` |
| `bob-2` | yes | `bob-2` |
| `Alice.Smith` | no | `alice-smith` |
| `-alice` | no | `alice` |
| `!!!` | no | `""` |
