# Programming conventions

- Go formatting and idiomatic packages; context cancellation at I/O boundaries; wrap errors with useful nonsecret context.
- Prefer standard packages and maintained focused dependencies over custom crypto, routing, parsers, scheduling or database engines. Lock versions in go.mod/go.sum. Record licenses when copying source.
- CGO_ENABLED=0 is mandatory for project programs, plugin hosts, tests and release builds. Do not use race builds (require CGO), native shared libraries or helper programs to bypass the rule.
- Host owns lifecycle, validation and commit; plugin owns collection policy. State changes only with committed revisions. File bytes and artifacts are untrusted.
- Explicit resource ownership; close handles; bounded buffers, network responses and temporary disk. Never log passwords, Authorization or full tokens.
- Configuration is YAML, read at startup. UI token records are runtime management data, not configuration or business snapshot data.
- Tests cover observable behavior, failure and recovery boundaries. Offline fixtures by default, no production upstream in CI.
- Make targets use overridable tools. Commits: type(scope): summary; breaking contracts use ! or BREAKING CHANGE.
- Update interfaces and behavior documentation alongside changes. Avoid speculative abstractions and broad dependency wrappers.
