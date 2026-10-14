# Security

Report vulnerabilities privately through [GitHub's private vulnerability reporting](https://github.com/cristiangirlea/harness/security/advisories/new).
Do not put credentials, private prompts, run records or sensitive file contents in public issues.

Harness runs coding agents and shell commands with access to your machine. Its command guard
is a guard rail, not an operating-system sandbox. Review the README's backend-specific
permission limits and use an isolated environment for untrusted repositories or tasks.

The watch dashboard listens on loopback by default. `--lan` lets others on the selected network
read run records without logging in, including prompts, paths and command output.
`--public` exposes those records through a Cloudflare Tunnel with Access login; see [watch exposure](docs/watch.md#behind-cloudflare-access).
Keep `--lan` on trusted networks and do not expose it to the public Internet.

CI and releases use standard GitHub-hosted runners. Public contributions must not run on
maintainers' desktops or private infrastructure. Tests use fake backends and do not need
provider credentials or paid model calls.
