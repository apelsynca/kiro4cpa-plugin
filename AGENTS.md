# kiro4cpa Plugin

## Commits

- Commit every change you make. Use Conventional Commit prefixes such as `fix:`, `feat:`, or `chore:`.

## Model Discovery

- Implement account-specific model discovery in the `kiro4cpa` plugin running inside CPA. Success means CPA exposes the models fetched for the plugin's own credential after a refresh.
- Do not require users to install or run Kiro CLI or Kiro IDE, or copy a model list from either. A working model picker in another client does not verify the plugin.
- Support Builder ID credentials without `profileArn` through a verified upstream listing method if one is available. Never invent a profile ARN or send a placeholder in a catalog request.
- Keep the default catalog as a fallback when upstream discovery fails. Showing defaults is not evidence that discovery worked.

## Local Environment

- CLIProxyAPI (CPA) is not installed on the user's local machine. Do not assume a local CPA server, config, or auth files exist, or ask the user to use them for verification. Use local plugin builds and tests; report live CPA verification separately.
