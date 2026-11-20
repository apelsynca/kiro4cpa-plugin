# kiro4cpa Plugin

## Commits

- Commit every change you make. Use Conventional Commit prefixes such as `fix:`, `feat:`, or `chore:`.

## Model Catalog

- Use Kiro's published model matrix to maintain the bundled per-plan catalog, including Student. Do not call upstream model discovery during credential refresh.
- Preserve per-account exclusions and use the Free catalog for unknown plans. A bundled list does not verify access for a particular credential.
- Do not require Kiro CLI or Kiro IDE, or require a profile ARN for model registration.

## Local Environment

- CLIProxyAPI (CPA) is not installed on the user's local machine. Do not assume a local CPA server, config, or auth files exist, or ask the user to use them for verification. Use local plugin builds and tests; report live CPA verification separately.
