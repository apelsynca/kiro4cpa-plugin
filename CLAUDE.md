# kiro4cpa-plugin Development Rules

## Plugin identity

CPA removes the extension from the library filename to determine the plugin ID and version:

- Production binaries must be named `kiro4cpa-v<version>.so` (or `.dylib` / `.dll`).
- Do not name the library `kiro4cpa.so`. CPA will register the ID `kiro4cpa` without a version.
- The `pluginID = "kiro4cpa"` constant in the code must match the filename prefix.

## Local debugging

On the Mac development environment, `make build-darwin` fails to link because the Xcode SDK's `.tbd` files do not recognize the `arm64e.x1` architecture. This is an environment issue, not a code issue. Use Docker for debugging. Replace `<version>` with the target plugin version:

```sh
make build-linux-docker
scp bin/linux/amd64/kiro4cpa.so aliyun-cloud:/tmp/
ssh aliyun-cloud 'cp /tmp/kiro4cpa.so \
  /docker/api4ai/cpa_data/plugins/linux/amd64/kiro4cpa-v<version>.so && \
  docker restart cpa'
```

The server runs CPA v8.0.42. Its plugin data directory, `/CLIProxyAPI/data`, is mounted at `/docker/api4ai/cpa_data/data` on the host. The management API can block IP addresses. Do not repeatedly guess the management key from the command line; use the management panel for administrative actions.

## Release process

1. Bump the version in `internal/kiro/version.go`. It must increase because the store compares version numbers.
2. Run `make test && go vet ./... && gofmt -l .`.
3. Commit the changes and push `main`.
4. Run `git tag -a v<version> -m "v<version>" && git push origin v<version>`.
5. CI runs the tests, builds five platform targets, and creates a GitHub Release.

**Pushing a tag publishes an official release.** Do not push tags while debugging.

## Official plugin store

`kiro4cpa` is already in the official registry (PR #287 was merged). To release a new version, push the tag. The store reads the latest GitHub Release; no new upstream PR is needed.

Update the official `registry.json` only when:

- The plugin's name, description, logo, or repository URL changes.
- A new plugin is added.

Keep this repository's `registry.json` as a fallback distribution source.

## Testing requirements

- `go test ./... -race` must pass.
- Keep test coverage for `internal/kiro` above 85%.
- Before committing, `gofmt -l .` must produce no output and `go vet ./...` must report no errors.
- CI runs tests on Linux, macOS, and Windows.

## Code conventions

- Implement only the Claude Messages executor. CPA converts OpenAI formats.
- The executor does not have the caller context and cannot wait for long periods. If a request would wait longer than the pacing limit, return 429 so CPA can switch accounts.
- Key credential state by `AuthID` (the filename), not by access token, so it survives token refreshes.
- Classify upstream errors by status: 402/403 trigger cooldown and failover, 429 uses CPA's backoff, and 5xx is reported as 502.
