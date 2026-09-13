# Agent instructions

Read docs/coding.md and docs/protocol.md before editing. Keep this repository independently buildable. Do not introduce dependencies on sibling workspace repositories. Use CGO_ENABLED=0 for every Go build and test; no native-library bypasses. Use mature libraries for mechanical work. Changes must include appropriate tests and accurate documentation. Run make check, make test, make build and make integration as applicable. Never claim an unexecuted check passed. Use Conventional Commits.

This application is source-agnostic. Do not introduce upstream-specific names, configuration fields, parsing or collection policy. Test with generic fixtures.

Preserve the project license Apache-2.0 and third-party licenses and attribution. Do not apply the project license to third-party dependencies or bundled components.
