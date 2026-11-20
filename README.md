# kiro4cpa-plugin

A Kiro provider plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) v8. It loads as a native `c-shared` plugin, connects Kiro accounts to CPA, and exposes Claude Messages, OpenAI Chat Completions, and OpenAI Responses endpoints.

> This is an unofficial project and is not affiliated with AWS or Kiro. Follow Kiro's terms of service.

## Features

Reference baseline checked on 2026-11-15: `ZyphrZero/kiro.rs` v0.9.9, commit `7dfb2c21dea5d2fa3618fdd603279d7fcef89074`. Its default branch had no additional commits at the time. The plugin already includes native Grok effort, reasoning history replay safeguards, retry on signature rejection, and safeguards for disabled thinking.

- Sign in from the browser with Builder ID, Google, GitHub, or IAM Identity Center, or import a refresh token or API key.
- Supported credential types: Social (Google or GitHub), IdC (Builder ID or IAM Identity Center), External IdP (such as Entra ID), and API Key.
- After sign-in, the plugin looks up the account email and uses it in the credential filename. Signing in again with the same account and method overwrites the existing file instead of creating a duplicate.
- Access tokens refresh automatically. If a request encounters an invalid token, the plugin forces one refresh and retries the request. Refreshed credentials are written back to the same file.
- During refresh, the plugin fetches and caches the account's available models. If the lookup fails, it falls back to a built-in list.
- Supports streaming and non-streaming responses, extended thinking, tools, and JPEG, PNG, GIF, and WebP images.
- Reports Kiro usage through CPA's QuotaProvider. Only active free-trial and bonus credits count toward available quota.
- The global `oauth-excluded-models` list applies to every account. Each account can add its own exclusions.
- Handles upstream errors by status code: CPA handles cooldown and failover for 401, 402, 403, and 429; upstream 5xx errors are reported as retryable 502s.
- Paces requests per credential and remembers monthly quota exhaustion and account suspensions reported by Kiro to avoid repeated upstream calls. A model-listing 403 does not disable the account; request-time suspension responses trigger a temporary cooldown instead.

The plugin implements only the Claude Messages executor. CPA handles conversion to and from OpenAI formats.

## Build

Requires Go 1.26 and cgo.

```bash
make test                # Run unit tests
make build-linux-docker  # Cross-compile bin/linux/amd64/kiro4cpa.so with Docker
make build-darwin        # Build bin/darwin/arm64/kiro4cpa.dylib locally (requires working Xcode Command Line Tools)
```

`build-linux-docker` cross-compiles with `gcc-x86-64-linux-gnu` inside `golang:1.26-bookworm`. The resulting binary requires glibc 2.34 or later.

CPA derives the plugin ID and version from the library filename. Deploy it as `kiro4cpa-v<version>.so` (or `.dylib` / `.dll`). A bare `kiro4cpa.so` registers `kiro4cpa` without a version. See [CONTRIBUTING.md](CONTRIBUTING.md) for development, testing, and release instructions.

## Install

### From the plugin store

Search for **Kiro OAuth** (ID `kiro4cpa`) in the CPA management panel's plugin store. The store downloads the release package for your OS and architecture.

If the store does not list it, add this repository as a store source in CPA's `config.yaml`:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/AkkunYo/kiro4cpa-plugin/main/registry.json"
```

### Manual install

Download `kiro4cpa_<version>_<os>_<arch>.zip` for your platform from [Releases](https://github.com/AkkunYo/kiro4cpa-plugin/releases), verify it against `checksums.txt`, and unzip it.

1. Put the library in the platform subdirectory of CPA's plugin directory (`plugins/` under CPA's working directory by default). Rename the extracted library to include the release version, for example:

   ```
   plugins/linux/amd64/kiro4cpa-v<version>.so
   ```

2. Enable the plugin in CPA's `config.yaml`:

   ```yaml
   plugins:
     enabled: true
     configs:
       kiro4cpa:
         enabled: true
         # Optional settings with their default values:
         # default_region: us-east-1   # Used when neither apiRegion nor profileArn specifies a region
          # client_version: 1.4.236    # Kiro IDE version reported in the User-Agent
         # os_version: darwin#24.6.0
         # node_version: 22.22.0
         # min_token_interval: 1s
         # max_token_interval: 2s
         # suspend_cooldown: 1h
   ```

3. Restart CPA. The log entry `plugin registered plugin_id=kiro4cpa` confirms that the plugin loaded.

## Placing credential files by hand

Signing in or importing credentials through [Web sign-in](#web-sign-in) is usually simpler. For bulk migration or scripting, place one JSON file per account in CPA's `auth-dir`, with `"type": "kiro4cpa"` at the top level:

```json
{
  "type": "kiro4cpa",
  "authMethod": "social",
  "refreshToken": "<refresh-token>",
  "region": "us-east-1"
}
```

| `authMethod` | Required fields |
|---|---|
| `social` | `refreshToken` |
| `idc` (also accepts `builder-id`, `iam`) | `refreshToken`, `clientId`, `clientSecret` |
| `external_idp` (also accepts `azuread`, `entra`) | `refreshToken`, `clientId`, `tokenEndpoint` (HTTPS) |
| `api_key` | `kiroApiKey` |

Optional fields: `accessToken`, `expiresAt`, `profileArn`, `authRegion`, `apiRegion`, `machineId`, `email`. Without `accessToken`, CPA refreshes the credential once immediately after loading it.

- kiro.rs stores its `credentials.json` as an array, which CPA does not support. Save each element as its own file and add the `type` field.
- Refreshed tokens and the model list are written back to the same file. Treat this file like an account password: restrict its permissions and never commit it.

## Models

`/v1/models` lists Kiro's model IDs, such as `claude-sonnet-4.5`, `claude-haiku-4.5`, and `auto`. To use a different name, configure a CPA `oauth-model-alias` under the `kiro4cpa` channel:

```yaml
oauth-model-alias:
  kiro4cpa:
    - name: claude-sonnet-4.5
      alias: claude-sonnet-4-5
      fork: true   # Keep the original ID as well
```

Account model exclusions accept original IDs, aliases configured in CPA, and `*` wildcards. For example, excluding `claude-sonnet-4-5` for a Pro account in the official panel prevents registration of the underlying `claude-sonnet-4.5` for that account. Other accounts are unaffected. An exclusion applies to the underlying model, including its original ID and all its aliases. An account-level alias overrides a channel-level mapping with the same alias. Global and account exclusions are additive; clearing the account's exclusions restores models not excluded globally.

## Quota

The plugin implements CPA's QuotaProvider. To query an account's Kiro quota through the CPA management API:

```http
GET /v0/management/plugins/kiro4cpa/quota?auth_index=<auth-index>
```

The official [Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) supports plugin quota providers. Some third-party panels only recognize built-in providers and will not show this quota.

Usage is measured in credits. Monthly quota, free-trial credits, and bonus credits show usage, limits, and the reset or expiration times reported by Kiro. The official panel uses quota bucket descriptions as row labels, so the plugin does not append the query time to each label. Authentication failures and quota lookup failures are reported separately from quota exhaustion and do not appear as zero quota.

## Request pacing

CPA can switch between accounts, but concurrent requests using one credential can still hit Kiro's rate limits. The plugin paces requests per credential and remembers upstream errors that should keep a credential out of rotation:

```yaml
plugins:
  configs:
    kiro4cpa:
      min_token_interval: 1s   # Minimum base gap between requests using one credential
      max_token_interval: 2s   # Maximum gap and maximum time a request can queue
      suspend_cooldown: 1h     # How long a suspended credential stays out of rotation
```

- The plugin draws a gap between `min_token_interval` and `max_token_interval`, adds up to 30% jitter in either direction, then caps the result at `max_token_interval`. Jitter helps keep separate instances from sending requests at the same time.
- Concurrent requests wait at most `max_token_interval`. A request that would wait longer receives a 429 so CPA can fail over.
- An upstream 402 (monthly quota exhausted) takes the credential out of rotation until the next UTC midnight.
- An upstream 403 containing `SUSPENDED` or `User is not authorized to make this call` takes the credential out of rotation for `suspend_cooldown`.
- Set `"disable_cooling": true` in an individual credential file to opt out of plugin pacing and cooldown. CPA's own cooldown still applies.

CPA remains responsible for credential selection, failover, and 429 backoff.

## Web sign-in

Start Kiro sign-in from the management panel's OAuth login page. The plugin's **Kiro sign in** page (`/v0/resource/plugins/kiro4cpa/connect`) opens in a new tab, where you can choose a method:

| Method | How to sign in |
|---|---|
| Builder ID | Open the AWS device authorization page, sign in with your Builder ID email, and confirm the authorization code. |
| Google / GitHub | Open the Kiro sign-in portal and choose Google or GitHub. After sign-in, the browser redirects to an unreachable `127.0.0.1:3128` page. This is expected. Copy its full address from the address bar and submit it on the plugin's sign-in page. |
| IAM Identity Center | Enter `https://<directory>.awsapps.com/start` and the region, then follow the AWS device authorization flow. |
| Import credentials | Import an existing refresh token or API key, as described below. |

After authorization, CPA saves the credential. The sign-in page checks CPA's official status API and only shows **Credentials saved.** once CPA confirms the save. If saving fails, the page shows the error. The original management panel tab does not need to remain open.

Submit the full Google or GitHub callback URL on the plugin's **Kiro sign in** page, not in the management panel's generic callback field. The generic field drops the callback path and `login_option` parameter that Kiro needs, and CPA does not pass the full URL to the plugin from that field.

Reloading the sign-in page restores the current method, authorization link, and device code, and shows the time remaining. If the management key becomes invalid, polling pauses until you enter a valid key and select **Verify key and continue**. Three consecutive network failures also pause polling; select **Check status again** to retry. If authorization fails or expires, start a new Kiro sign-in from the management panel to create a new CPA session.

**Import credentials** supports three types:

| Credential type | Required fields | Where to find them |
|---|---|---|
| Social Refresh Token (Google / GitHub) | Refresh Token, Region | `refreshToken` from a Kiro IDE or kiro.rs credential |
| IdC Refresh Token (Builder ID / enterprise SSO) | Refresh Token, Client ID, Client Secret, Region | `refreshToken` in the AWS SSO cache (`~/.aws/sso/cache/`) and `clientId` and `clientSecret` in the client registration file in the same directory |
| Kiro API Key | API Key | An API key generated in the Kiro console |

Region defaults to `us-east-1`. The import checks required fields, then attempts an OAuth token refresh or an API key quota lookup. The page distinguishes verified, invalid, and temporarily unverifiable credentials. Even if verification fails, the credential can still be sent to CPA; CPA's save result is authoritative. Failure to look up the account email does not block import.

The sign-in page uses CPA's management API and requires the management key. If you selected **Remember password** when signing in to the management panel and both pages use the same address, the sign-in page reads the remembered key. Only credentials stored in the official panel's current `enc::v2::` format are supported. Enter the key manually if no supported key was remembered or the stored key is in an older or plaintext format. Manually entered keys are not saved.

The sign-in page supports the official panel's warm gray, white, dark, and automatic themes. The plugin UI is in English; server error details are displayed as returned. **Return to management panel** opens the official OAuth login page without relying on `window.opener`.

Credential files are named `kiro4cpa-<method>-<email>.json`, for example `kiro4cpa-google-user@example.com.json`. If the email is unavailable, the plugin uses a hash of the account ID. Signing in again with the same account and method overwrites the file.

## Reliability and compatibility

- Request execution, quota lookups, and scheduled refreshes share a per-account refresh coordinator. The plugin serializes its own credential writes per account; CPA persists scheduled refresh results separately. If a plugin write fails, the coordinator retains the latest token and retries the write on a later request. It retains at most 64 old credential fingerprints per account. Snapshots older than that retained history fail instead of trying to reuse an old token. The cache is process-local and does not replace CPA's persistent storage.
- Corrupt, truncated, or unparseable upstream events fail explicitly instead of ending the stream with a successful `message_stop`. Tool arguments accumulate by call ID, including interleaved fragments and late-arriving tool names. The first tool can stream immediately, while output tool blocks are kept in order for CPA's Responses converter. Invalid JSON, non-object arguments, and nonempty arguments without a stop event fail rather than being replaced with an empty object. Calls without arguments may omit the stop event.
- Streaming and non-streaming responses use the same stop-reason rules, preserving upstream truncation, context overflow, and refusal signals. Thinking-only output is not automatically treated as truncated, and a refusal during tool output aborts the response. The plugin reads fragmented HTTP error bodies up to a size limit before classifying them so split token-expiration messages still trigger a refresh.
- Consecutive trailing user messages are merged into the current turn. The plugin preserves images and tool results without inserting a fictitious assistant "OK". Tool descriptions retain client instructions without injecting forced Write/Edit chunking or instructions to suppress progress updates. Kiro's name and description limits and schema normalization still apply.
- The protocol converter handles messages, tool definitions and results, Base64 images, and thinking. It does not forward or enforce `max_tokens`, `temperature`, `top_p`, `top_k`, `stop_sequences`, `tool_choice`, or structured output schemas. Do not rely on these settings to limit output or force tool selection.
- Native effort follows `ZyphrZero/kiro.rs` v0.9.9. Known supported Claude models use `additionalModelRequestFields.output_config.effort`; GPT 5.6 sol/terra/luna and Grok 4.7 use `reasoning.effort`. For Grok, `max` maps to `xhigh`. Unknown models use prompt-based adaptation. Explicit effort takes precedence over effort derived from the thinking budget. Opus 4.6 receives native fields only for explicitly adaptive requests, and `thinking: disabled` takes precedence. Upstream token usage takes priority; local output estimates use the cumulative character count so the number of fragments does not affect the result.
- Requests with native effort omit duplicate thinking instructions. Legacy models and Opus 4.6 in enabled mode still use prompt-based instructions. When thinking is disabled, native reasoning is not emitted as ordinary response text, even when the response contains only reasoning.
- Reasoning history uses a separate `reasoningContent` field. A merged assistant turn replays only a single thinking block with a valid non-placeholder signature, or a single valid Base64 redacted block. Multiple, incomplete, or unsigned blocks are omitted without losing text or tools. Only an explicit HTTP 400 `THINKING_SIGNATURE_INVALID` triggers a retry without that field, using the same single-retry budget as token recovery. Local protocol tests cover history signature replay; acceptance by Kiro has not been verified with real credentials.
- Kiro does not support assistant prefill, so trailing assistant prefill is dropped. Unsupported content types such as image URLs and documents are not forwarded.

## Known limitations

- Builder ID, Google, and GitHub sign-in have been tested end to end. IAM Identity Center, External IdP, and API Key are implemented to spec.
- Release binaries cover `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, and `windows/amd64`. Only `linux/amd64` and `darwin/arm64` have been tested end to end.

## Acknowledgements

These MIT-licensed projects informed the protocol implementation. See [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES) for their copyright notices:

- [hank9999/kiro.rs](https://github.com/hank9999/kiro.rs), [ZyphrZero/kiro.rs](https://github.com/ZyphrZero/kiro.rs)
- [xiaokui-dev/cliproxyapi-kiro-plugin](https://github.com/xiaokui-dev/cliproxyapi-kiro-plugin)
- [nguyenha935/kiro-cpa-plugin](https://github.com/nguyenha935/kiro-cpa-plugin)

## License

[MIT](LICENSE)
