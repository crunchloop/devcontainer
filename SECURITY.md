# Security Policy

## Supported versions

The latest minor release line receives security fixes. Pre-1.0 releases
do not carry long-term support guarantees; once 1.0.0 ships, this policy
will be updated to define an LTS window.

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |
| < 0.1   | :x:                |

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub
issues, discussions, or pull requests.**

Instead, report them privately via GitHub's
[private vulnerability reporting](https://github.com/crunchloop/devcontainer/security/advisories/new)
feature. This creates a draft security advisory visible only to the
maintainers and the reporter.

If for any reason you cannot use private vulnerability reporting, email
**security@crunchloop.io** with the subject prefix
`[devcontainer-go security]`.

When reporting, please include as much of the following as you can:

- A description of the issue and the kind of impact you believe it has
  (e.g. arbitrary host command execution, container escape, sensitive
  data exposure, denial of service).
- The version (commit SHA or tag) the issue reproduces on.
- A minimal reproduction: a `devcontainer.json`, feature ref, compose
  file, or code snippet that triggers the behavior.
- Any proof-of-concept exploit code, if available.
- The impact on confidentiality, integrity, or availability.
- Whether the issue is already public elsewhere.

You are not required to provide a fix, but suggested mitigations are
welcome.

## What to expect

- **Acknowledgement** within 3 business days of receipt.
- **Triage and severity assessment** within 7 business days, including
  whether the report is in scope.
- **Status updates** at least every 14 days while the issue is open.
- **Coordinated disclosure**: once a fix is available, we will publish
  a GitHub Security Advisory and credit the reporter (unless they
  prefer to remain anonymous). For high-severity issues we aim to ship
  a patch release within 30 days of validated triage.

## Scope

In scope:

- The `github.com/crunchloop/devcontainer` module and its sub-packages.
- The default `runtime/docker` implementation.
- The default `feature` resolvers (OCI, HTTPS, local) and feature
  install pipeline.

Out of scope:

- Vulnerabilities in third-party dependencies (please report those
  upstream; we will update the dependency when a fix lands).
- Vulnerabilities in Docker, BuildKit, the Docker Compose plugin, or
  the host operating system.
- Issues in `devcontainer.json` configurations that explicitly opt into
  host-side execution (`UpOptions.RunInitializeCommand`,
  `UpOptions.RunSecretsCommand`) — the documented contract is that
  these run arbitrary host commands and must only be enabled for
  trusted input.

## Hardening notes for embedders

When embedding the library in a service that runs untrusted
`devcontainer.json` input, the following defaults reduce attack surface:

- Leave `UpOptions.RunInitializeCommand` and `UpOptions.RunSecretsCommand`
  unset (default `false`). Both execute arbitrary host commands and
  require an explicit `EngineOptions.HostExecutor` to do anything.
- Pin features by digest (`feature@sha256:…`) rather than mutable tags
  to defend against compromised upstream feature repositories.
- Configure `EngineOptions.OCIKeychain` to scope registry credentials
  to the registries you trust; the default
  (`authn.DefaultKeychain`) reads ambient Docker credentials.
- Treat the `Workspace.Config` returned to your code as authoritative
  for what will run; inspect `customizations.<tool>` namespaces if
  your tool consumes them.
