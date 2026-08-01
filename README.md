# LingoBridge

WeChat/Feishu/GitHub Bot → LLM direct bridge. Connects chat bot and PR review accounts to OpenAI/Anthropic-compatible LLM APIs.

## Quick Start

### 1. Build

```bash
go build -o lingobridge ./cmd/lingobridge/
```

### 2. Configure

```bash
cp config.yaml.example ~/.lingobridge/config.yaml
# Edit ~/.lingobridge/config.yaml with your LLM API key and settings
```

If `~/.lingobridge/config.yaml` does not exist, the first real command you run
starts an interactive setup flow. It asks for at least one model profile and
sets that first profile as `llm.default_model`.

Minimal config:

```yaml
llm:
  default_model: "deepseek"
  models:
    deepseek:
      provider: "openai"
      base_url: "https://api.deepseek.com/v1"
      api_key: "sk-your-key-here"
      id: "deepseek-chat"
```

You can also add model profiles from the CLI:

```bash
./lingobridge model add gpt4o \
  --provider openai \
  --base-url https://api.openai.com/v1 \
  --api-key sk-your-openai-key \
  --id gpt-4o \
  --endpoint responses \
  --context-window 128000
```

### 3. Add a bot account

Scan the QR code with your WeChat app:

```bash
./lingobridge account new weixin --name mybot
```

Or add a Feishu self-built app account:

```bash
./lingobridge account new feishu --name fsbot --app-id cli_xxx --app-secret your-app-secret
```

If Feishu credentials are omitted, LingoBridge prompts for them interactively:

```bash
./lingobridge account new feishu --name fsbot
```

Feishu app credentials are saved under `platforms.feishu.accounts` in the
shared `~/.lingobridge/config.yaml`. Those config entries are the Feishu
account source used by `account list` and `run`.

Or add a GitHub App PR review account:

```bash
./lingobridge account new github \
  --name reviewer \
  --app-id 123456 \
  --installation-id 987654 \
  --private-key-path /etc/lingobridge/github-app.pem \
  --repo owner/repo \
  --poll-interval 2m
```

GitHub App credentials and repository allowlists are saved under
`platforms.github.accounts`, and those config entries are the GitHub account
source used by `account list` and `run`. Before running the GitHub account,
explicitly set `platforms.github.accounts.<name>.mcp.command` and `.mcp.args`
to point at your GitHub MCP server. LingoBridge does not write or assume
default GitHub MCP command arguments.

For Feishu, enable bot capability and long-connection event subscription for
`im.message.receive_v1` in the Feishu Open Platform app console. Add any
extra event subscriptions, such as `p2p_chat_create` or
`im.chat.access_event.bot_p2p_chat_entered_v1`, only when they are listed under
`platforms.feishu.events` with an explicit `version`. The first version
supports text messages in 1:1 chats and group messages that mention the bot.

### 4. Run

```bash
./lingobridge run
```

Listens to all enabled accounts concurrently. If no enabled accounts exist yet,
it stays running and waits for a later account reload. Use `--account` to run a
specific one, and `--verbose` to set the log level (`all`, `debug`, `info`,
`warn`, or `error`; default `info`). Use `all` to include Feishu SDK debug logs:

If all active account monitors exit because of a non-cancellation error, such as
invalid account or platform config, `run` exits and prints the monitor error.

```bash
./lingobridge run --account mybot
./lingobridge run --verbose debug
```

Logs are printed as `timestamp - [LEVEL] - [component] message`; Feishu SDK
logs use the `feishu/lark` component.

While `run` is active, `account new` and `account delete` notify it over a local Unix socket so account changes are applied without restarting the bot loop. If no running process is reachable, the CLI prints a `Note:` and the account/config change still succeeds.
`model add` also notifies the running process. On reload, LingoBridge reloads
`config.yaml`, rebuilds the active model list, and restarts account monitors
when relevant config changes.

## CLI Reference

| Command | Description |
|---|---|
| `account new weixin [--name <name>]` | Add a WeChat bot account via QR login and reload a running bot process |
| `account new feishu [--name <name>] [--app-id <id>] [--app-secret <secret>] [--base-url <url>]` | Add a Feishu self-built app account, write Feishu config, and reload a running bot process |
| `account new github [--name <name>] [--app-id <id>] [--installation-id <id>] [--private-key-path <pem>] --repo owner/repo [--repo owner/other] [--poll-interval <duration>] [--base-url <url>] [--web-url <url>]` | Add a GitHub App PR review account, write GitHub config, and reload a running bot process |
| `account list` | List all accounts as `platform/name` with their account ID |
| `account delete <name\|platform/name>` | Delete an account from its platform-owned account source, clear its sync cursor, and reload a running bot process |
| `model add <name> [--provider <openai\|anthropic>] [--base-url <url>] [--api-key <key>] [--id <model-id>] [--endpoint <mode>] [--context-window <tokens>] [--compact <true\|false\|auto>] [--compact-threshold <ratio>] [--compact-instructions <text>] [--default]` | Add an LLM model profile to config and optionally make it the default |
| `run [--account <name>] [--verbose <all\|debug\|info\|warn\|error>]` | Start the bot loop with optional log level, default `info` |

`account delete <name>` works when only one account has that name. If multiple
platforms have the same name, delete with the `platform/name` shown by
`account list`, such as `feishu/default` or `wechat/default`.

## In-Chat Commands

Send these as WeChat or Feishu text messages to the bot:

| Command | Description |
|---|---|
| `/help` | Show Markdown-formatted in-chat commands and current platform tool summaries |
| `/current` | Show current session and model |
| `/new [name]` | Create a new conversation session |
| `/list` | List your sessions |
| `/switch <name>` | Switch current session |
| `/rename <name>` | Rename current session |
| `/archive [name]` | Archive a session |
| `/clear` | Archive the current session and start a new one |
| `/model [name]` | Show or switch model profile |
| `/compact` | Manually compact the current session context |

Platforms can narrow or extend the shared command set through their platform
definition. The current WeChat and Feishu adapters both enable the default
shared commands listed above.

## Message Handling

### WeChat

When a user replies to a quoted WeChat text message, LingoBridge includes the
quoted context in the message sent to the LLM:

```text
[引用: quoted text]
current message
```

Quoted media is not downloaded or interpreted; only the current text is sent.

Current image messages are downloaded from WeChat media/CDN and passed to the
selected model through LingoBridge's provider-neutral attachment interface. With
OpenAI Responses model profiles, images are first saved under the current
platform's `data/media/{user}/{session}/`, then uploaded to the OpenAI Files API with
`purpose=vision` and sent as `input_image` parts. The JSONL history stores both
a provider reference (`ref_provider`, `ref_type`, `ref_id`) and `local_path`, so
later turns can still refer to the image while that message remains inside
`llm.max_history`.

Images generated by OpenAI Responses model profiles are also saved under the
same per-user, per-session media directory and uploaded to the OpenAI Files API.
Their history attachments use the returned `file_...` reference instead of
`image_generation_call`, because Responses persistence is not required for image
continuity. If uploading a generated image to the OpenAI Files API fails, the
image is still sent to WeChat and saved in history with an empty `ref_id`; that
attachment is not sent as future image context until a valid `file_...` ref is
available. OpenAI Responses requests are sent with `store: false`; legacy
`image_generation_call` entries already in history are not sent back as image
context. Non-Responses model profiles keep the legacy text history format for
generated images.

Image understanding currently requires an OpenAI-compatible model profile with
`endpoint: "responses"`. If a user sends only an image, LingoBridge uses
`请描述这张图片。` as the user prompt.

Long text replies are automatically split into multiple WeChat messages before
sending. When provider-native context compaction starts, WeChat sends a progress
text message; after compacted context is saved, it sends the compact success
summary as another text message.

### Feishu

Feishu support uses a self-built app long connection. In 1:1 chats, text and
rich text messages are processed. In group chats, LingoBridge responds only to
messages that explicitly mention the current bot. On startup, LingoBridge
resolves the current Feishu bot `open_id`; if that lookup fails, the Feishu
account does not start. Incoming group messages that mention all members are
ignored. Other group messages remove only mention tokens that target that bot
`open_id`, so mentions of other users or other bots are preserved. Other
incoming mentions are shown to the LLM as readable `@Name` text. If the final
LLM reply contains a unique `@Name` from the triggering message, LingoBridge
converts it back to a Feishu mention; unmatched or ambiguous names stay as
plain text. Incoming
Feishu rich text (`post`) messages are converted to Markdown before they are
sent to the LLM; embedded rich text images, media, files, and emoji are
represented with text placeholders rather than downloaded.
LLM text replies are streamed by updating one Feishu rich text message in
place. In-chat command replies, event command output, unsupported-message
notices, and generated-image notices are still sent as normal one-shot
messages. Feishu outbound text is sent as rich text markdown content (`post`
with `md`), using the core reply text without the WeChat markdown filter. Long
Feishu replies are
split into multiple streamed rich text
messages as they are generated, and each message keeps its own edit budget.
In group chats, bot responses quote the triggering Feishu message; in 1:1 chats,
responses are still sent as normal messages.
Stream previews slow down as the reply grows and are capped to stay within
Feishu's per-message edit limit. The final update is prioritized; if Feishu
still rejects it, LingoBridge sends the final answer as a new rich text message.
Long text splitting prefers line boundaries; WeChat keeps a 4000-character
limit, while Feishu rich text uses a 25 KiB threshold and splits individual
over-limit lines only at UTF-8 safe boundaries.
The built-in Feishu runtime enables this code-level streaming path explicitly.
Custom integrations can enable it by setting `core.Bot.EnableTextStreaming` to
`true` and making their sender implement the optional `core.TextStreamSender`
interface. Senders that do not implement that optional interface automatically
fall back to normal chunked text sends. When provider-native context compaction
starts, Feishu sends one progress rich text message; after compacted context is
saved, LingoBridge updates that progress message to the compact success summary
and marks the original triggering message with a `DONE` reaction instead of
sending an extra success message.
Extra Feishu events are registered from `platforms.feishu.events`. Each event
item has `name`, `version`, and `run`; `run` may be one shell command string or
a list of shell command strings. `version: "1.0"` events are registered with
Feishu SDK `OnCustomizedEvent`; `version: "2.0"` events are registered through
LingoBridge's built-in event-name to `OnP2...` mapping. Configured v2.0 events
currently support `im.chat.access_event.bot_p2p_chat_entered_v1`. Non-empty
stdout from each command is sent back as a Feishu rich text message only when
the event contains a `chat_id`.
Event commands receive environment variables including
`LINGOBRIDGE_EVENT_NAME`, `LINGOBRIDGE_EVENT_JSON`,
`LINGOBRIDGE_COMMAND_HELP`, and Feishu-specific fields such as
`LINGOBRIDGE_FEISHU_CHAT_ID`.

For example, a configured `p2p_chat_create` hook can send a first-chat greeting
followed by the in-chat command help:

```yaml
platforms:
  feishu:
    events:
      - name: p2p_chat_create
        version: "1.0"
        run:
          - "printf '你好，我是 LingoBridge。直接发送问题即可开始对话。'"
          - "printf '%s' \"$LINGOBRIDGE_COMMAND_HELP\""
```

LingoBridge can expose global Model Context Protocol servers as LLM tools.
Configure them under top-level `mcp.servers`. Supported transports are
`stdio` for local command-based servers and `streamable_http` for remote MCP
HTTP endpoints. MCP tools are available to every platform/account that uses a
tool-capable model profile. Omit `scope` to expose a server globally, or set
`scope.platforms` / `scope.accounts` to limit it to specific bots. Account
selectors support either `platform/account_name` or the stable stored account ID
such as `feishu:cli_xxx`.

MCP tool names are always prefixed as `mcp_<server>_<tool>` after safe-name
normalization, for example `mcp_filesystem_read_file`. If an MCP server cannot
start, list tools, or serve a tool call, LingoBridge logs the degraded behavior
and continues running other servers, platform tools, and normal chat.

```yaml
mcp:
  servers:
    filesystem:
      transport: stdio
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
      scope:
        platforms: ["feishu"]
        accounts:
          - "feishu/fsbot"
          - "feishu:cli_xxx"
    remote_docs:
      enabled: false
      transport: streamable_http
      url: "https://example.com/mcp"
      headers:
        Authorization: "Bearer your-token"
```

Feishu can expose platform-level document tools to tool-capable LLM profiles.
Configure shared tool limits under `platforms.feishu.tools` and enable the
document tool package under `platforms.feishu.tools.docs`. They are disabled by
default and read-only when enabled. The first version registers
`feishu_docs_search` and `feishu_docs_read`; `feishu_docs_create` and
`feishu_docs_append` are registered only when `allow_write: true` and
`allowed_folder_tokens` is non-empty. These tools use the Feishu self-built app
tenant token, so access follows the app's document permissions rather than the
asking user's personal permissions. Strict per-user document access requires a
separate Feishu OAuth flow.

Feishu can also expose `feishu_chat_history_get` to read recent messages from
the Feishu chat that triggered the current LLM turn. Enable it under
`platforms.feishu.tools.chat_history`. The runtime binds the trusted current
`chat_id`; the model cannot provide or switch to another chat ID. The optional
`limit` argument defaults to 20 and is capped at 100. Results are fetched from
the Feishu message history API, returned in chronological order, and bounded by
`platforms.feishu.tools.max_chars`. Text and rich-text post messages are
rendered as readable content; other message types use safe placeholders rather
than exposing file or image keys.

Chat history is disabled by default. Reading p2p history requires the app's
normal message-history capability. Reading group history additionally requires
the Feishu permission shown as `获取群组中所有消息`, and the bot must be a member
of that group. API or permission failures are returned to the model as tool
errors with an actionable group-permission hint.

Feishu can also expose `feishu_litellm_invite_create` for LiteLLM account
requests. Enable it under `platforms.feishu.tools.litellm` and use a
tool-capable model profile. The user must provide both `邮箱` and `申请原因` in
natural language. The tool writes those fields plus the Feishu sender to the
configured Bitable Person field, creates a LiteLLM user through `/user/new` with
the sender name as `user_alias` when the app can read it, creates an invitation
through `/invitation/new`, and returns a password setup link as
`[Invitation Link](<litellm_base_url>/ui?invitation_id=<id>)`.
The owner Person field uses the message sender's `open_id`; resolving the
sender display name for LiteLLM `user_alias` requires the Feishu app to have
permission to read basic contact user information.

Feishu conversation history is isolated per user in 1:1 chats and shared by
`chat_id` in group chats, so everyone who mentions the bot in the same group
continues the same group session.

Feishu image, file, video, and voice messages are acknowledged with an
unsupported-message notice in this first version. Generated images from the LLM
are not sent back to Feishu yet.

### GitHub

GitHub support uses a GitHub App installation token and polls configured
repositories for open pull requests. Draft PRs are skipped. A PR is reviewed
when it first appears or when its `head.sha` changes; unchanged PRs are tracked
through the platform `sync_cursors` buffer and are not reviewed again.

Review instructions are read only from `.github/review_instructions.md` in the
base repository at the PR base SHA. Instructions from the head branch are never
trusted or used as fallback. If the base file is missing and
`platforms.github.accounts.<name>.review.default_instructions` is configured,
that default text is used for the review. If no default is configured, that PR
SHA is marked `missing_instructions` and retried only after the head SHA
changes.

Each review starts a fresh GitHub MCP host from
`platforms.github.accounts.<name>.mcp`. The server id is fixed to `github`, so
MCP tools are exposed to the LLM as `mcp_github_<tool>`, such as
`mcp_github_pull_request_read`. LingoBridge injects the short-lived GitHub App
installation token as `GITHUB_PERSONAL_ACCESS_TOKEN` and, when configured,
injects `GITHUB_HOST` from `web_url`.

The GitHub platform wraps configured MCP tools with PR-review guards. Tool calls
must target the current PR. File reads are limited to the current base/head
repositories and current PR base/head SHA or branch refs, including
`refs/pull/<number>/head` on the base repository, and callers must not pass both
`sha` and `ref`. Pull request reads are limited to `get`, `get_diff`,
`get_files`, `get_status`, and `get_check_runs`; comment, commit, historical
review, and review-comment reads are rejected. Full PR diff reads may be used
for small PRs; if GitHub reports the diff is too large, automated review should
switch to paginated PR file reads starting with a small page size such as 30 or
50. Review writes can only create a pending review without an event, add inline
comments to that pending review, and submit that review once as `COMMENT`.
Approvals, request-changes reviews, thread resolution, PR updates, branch
updates, merges, and repository writes are rejected before reaching the MCP
server.

Automated GitHub reviews use a dedicated review system prompt. Trusted review
instructions come only from the base-repo file above or from the configured
default; PR metadata, title/body, diffs, changed files, and tool output are
treated as untrusted context. PR title/body text is lightly sanitized before it
is placed in the prompt: hidden HTML comments/attributes, invisible/control
characters, markdown image alt text, markdown link titles, and GitHub token-like
strings are removed or redacted.

The review flow is deliberately high signal: gather PR context, triage changed
files by risk, check correctness/regressions, security, performance/resource
handling, test coverage, and documentation/config accuracy, then publish only
actionable findings that are worth showing. If there are no actionable findings,
the bot still submits a `COMMENT` review summary such as
`No actionable issues found.` If tool failures or timeouts prevent meaningful
diff inspection, the bot does not submit a GitHub review and does not mark the
PR SHA as reviewed.

Global MCP servers are not merged into automated GitHub reviews. Each review
uses only the per-review GitHub MCP host from the account configuration and the
guarded tools exposed for the current PR.

## Configuration

`~/.lingobridge/config.yaml`:

| Field | Default | Description |
|---|---|---|
| `llm.default_model` | `deepseek` | Default model profile name |
| `llm.models.<name>.provider` | — | `"openai"` or `"anthropic"` |
| `llm.models.<name>.base_url` | — | LLM API base URL |
| `llm.models.<name>.api_key` | — | API key for this model profile |
| `llm.models.<name>.id` | — | Provider model ID, such as `deepseek-chat` or `gpt-4o` |
| `llm.models.<name>.endpoint` | `chat` | Endpoint mode: `chat` or `responses` for OpenAI-compatible APIs, `messages` for Anthropic |
| `llm.models.<name>.context_window` | — | Model context window in tokens; required for native-compact-capable endpoints when compact mode is `true` or `auto` |
| `llm.models.<name>.compact.mode` | `auto` | Native compact mode: `true`, `false`, or `auto` |
| `llm.models.<name>.compact.threshold` | `0.9` | Auto compact threshold as a fraction of `context_window` |
| `llm.models.<name>.compact.instructions` | — | Optional provider instructions for what compacted context should preserve |
| `llm.system_prompt` | `"You are a helpful assistant."` | System prompt |
| `llm.max_history` | `0` | Max historical messages per request. `0` = no limit |
| `mcp.servers.<name>.enabled` | `true` | Enable this global MCP server; disabled servers are ignored |
| `mcp.servers.<name>.transport` | — | MCP transport: `stdio` or `streamable_http` |
| `mcp.servers.<name>.command` | — | Command to start a `stdio` MCP server |
| `mcp.servers.<name>.args` | `[]` | Arguments passed to the `stdio` command |
| `mcp.servers.<name>.env` | `{}` | Extra environment variables passed to the `stdio` command |
| `mcp.servers.<name>.cwd` | — | Optional working directory for the `stdio` command |
| `mcp.servers.<name>.url` | — | Absolute HTTP(S) URL for a `streamable_http` MCP server |
| `mcp.servers.<name>.headers` | `{}` | Static HTTP headers for a `streamable_http` MCP server; prefer headers over URL query secrets |
| `mcp.servers.<name>.scope.platforms` | `[]` | Optional platform IDs allowed to see this MCP server's tools; omitted scope is global |
| `mcp.servers.<name>.scope.accounts` | `[]` | Optional account selectors allowed to see this MCP server's tools; entries may be `platform/account_name` or stable account ID |
| `platforms.<platform>` | — | Platform-private config block; each platform owns its internal schema |
| `platforms.github.accounts.<name>.app_id` | — | GitHub App ID for account `<name>` |
| `platforms.github.accounts.<name>.installation_id` | — | GitHub App installation ID used to create installation tokens |
| `platforms.github.accounts.<name>.private_key_path` | — | Local PEM private key path for signing GitHub App JWTs |
| `platforms.github.accounts.<name>.base_url` | `https://api.github.com` | GitHub REST API base URL |
| `platforms.github.accounts.<name>.web_url` | `https://github.com` | GitHub web URL and MCP `GITHUB_HOST` value |
| `platforms.github.accounts.<name>.poll_interval` | `2m` | Interval between PR polling passes |
| `platforms.github.accounts.<name>.repositories` | — | Repository allowlist in `owner/repo` form; at least one is required |
| `platforms.github.accounts.<name>.review.max_tool_calls` | `30` | Tool-call limit for one automated PR review |
| `platforms.github.accounts.<name>.review.tool_timeout` | `30s` | Per-tool timeout for one automated PR review |
| `platforms.github.accounts.<name>.review.tool_result_limit` | `60000` | Maximum tool result characters returned to the LLM per call |
| `platforms.github.accounts.<name>.review.default_instructions` | — | Optional default review instructions used only when `.github/review_instructions.md` is missing from the base repository at the PR base SHA |
| `platforms.github.accounts.<name>.mcp.command` | — | Required command used to start the per-review GitHub MCP server |
| `platforms.github.accounts.<name>.mcp.args` | — | Required arguments for the per-review GitHub MCP server; include explicit `--tools=...` |
| `platforms.github.accounts.<name>.mcp.env` | `{}` | Extra MCP server environment variables; GitHub tokens are injected automatically and should not be configured here |
| `platforms.github.accounts.<name>.mcp.cwd` | — | Optional working directory for the per-review GitHub MCP server |
| `platforms.feishu.accounts.<name>.app_id` | — | Feishu app ID for account `<name>` |
| `platforms.feishu.accounts.<name>.app_secret` | — | Feishu app secret for account `<name>` |
| `platforms.feishu.accounts.<name>.base_url` | `https://open.feishu.cn` | Feishu Open Platform base URL |
| `platforms.feishu.events[].name` | — | Extra Feishu event to register; v1.0 supports customized event names such as `p2p_chat_create`, and v2.0 currently supports `im.chat.access_event.bot_p2p_chat_entered_v1` |
| `platforms.feishu.events[].version` | — | Required Feishu event protocol version: `"1.0"` uses `OnCustomizedEvent`; `"2.0"` uses a built-in `OnP2...` mapping |
| `platforms.feishu.events[].run` | — | Shell command string or list of shell command strings to run for the event |
| `platforms.feishu.tools.max_results` | `5` | Shared maximum result count for Feishu tools that return lists, including `feishu_docs_search` |
| `platforms.feishu.tools.max_chars` | `12000` | Shared maximum character count for Feishu tools that return content, including `feishu_chat_history_get` and `feishu_docs_read` |
| `platforms.feishu.tools.allowed_folder_tokens` | `[]` | Shared folder token allowlist for Feishu tools; currently used for Docs write tools and optional search narrowing |
| `platforms.feishu.tools.allowed_space_ids` | `[]` | Shared Wiki space ID allowlist for Feishu tools; currently used for Docs search narrowing |
| `platforms.feishu.tools.chat_history.enabled` | `false` | Enable `feishu_chat_history_get` for the current trusted Feishu `chat_id`; each call returns at most 100 messages |
| `platforms.feishu.tools.docs.enabled` | `false` | Enable Feishu Docs tools for tool-capable LLM profiles |
| `platforms.feishu.tools.docs.allow_write` | `false` | Register Feishu Docs create/append tools when enabled and folder allowlist is configured |
| `platforms.feishu.tools.litellm.enabled` | `false` | Enable the Feishu natural-language LiteLLM account invitation tool |
| `platforms.feishu.tools.litellm.base_url` | — | LiteLLM proxy base URL used for API calls and invitation link construction |
| `platforms.feishu.tools.litellm.api_key` | — | LiteLLM admin/master API key used for `/user/new` and `/invitation/new` |
| `platforms.feishu.tools.litellm.user_role` | `internal_user` | LiteLLM role sent when creating the user |
| `platforms.feishu.tools.litellm.bitable.app_token` | — | Target Bitable Base token, not the Feishu Open Platform App ID; auth reuses `platforms.feishu.accounts.<name>` |
| `platforms.feishu.tools.litellm.bitable.table_id` | — | Target Bitable table ID used to record account requests |
| `platforms.feishu.tools.litellm.bitable.email_field` | `邮箱` | Bitable field receiving the applicant email |
| `platforms.feishu.tools.litellm.bitable.reason_field` | `申请原因` | Bitable field receiving the application reason |
| `platforms.feishu.tools.litellm.bitable.owner_field` | `所有者` | Bitable Person field receiving the Feishu user who sent the request |
Each model profile is independent. `provider`, `base_url`, `api_key`, and `id` are required; `endpoint` is optional and defaults to `chat`.
For Anthropic model profiles, an omitted `endpoint` defaults to `messages`.
Native context compaction defaults to `compact.mode: auto`. OpenAI-compatible
profiles support native compact only with `endpoint: "responses"`; Anthropic
profiles support it with `endpoint: "messages"` on Anthropic models that
currently expose provider-native compaction. When the current provider supports
native compact and the estimated request context exceeds
`context_window * compact.threshold`, LingoBridge compacts older history,
stores the returned provider-owned items under
`provider_contexts.<modelProfile>`, and sends those items before later model
input. Successful compaction emits the platform-specific progress notice
described in Message Handling. `/compact` manually asks the current provider to
compact the session:
OpenAI Responses profiles call the provider compact endpoint directly, while
Anthropic profiles rely on the provider's native compaction trigger and may not
emit compacted context below that threshold. If `compact.mode: false`,
automatic and manual compaction are disabled; if `compact.mode: true`,
unsupported provider endpoints are rejected during config validation. Chat
Completions-style endpoints do not receive a custom summarization fallback.
Top-level `llm.model`, `llm.provider`, `llm.base_url`, `llm.api_key`, and `llm.endpoint` are no longer supported.
The core config loader preserves `platforms.<platform>` as platform-private
YAML and only validates that platform keys are safe registry IDs. Platform
packages decode and validate their own config through core's scoped platform
config API.
On startup and reload, `run` validates the default model profile and resets any
saved per-user model preference that no longer exists back to
`llm.default_model`.

## Storage

```
~/.lingobridge/
  config.yaml                          # Shared LLM/MCP config and platform-private platforms.<platform> config
  lingobridge.sock                       # Local control socket used by a running process
  platforms/
    wechat/
      data/
        lingobridge.db                   # WeChat accounts, sessions, user preferences, sync cursors
        sessions/{userId}/{sessionId}.jsonl # Conversation snapshots; may include compact provider_contexts and tool_traces
        media/{safeUserId}/{safeSessionId}/
    feishu/
      data/
        lingobridge.db                   # Feishu sessions, user preferences, sync cursors, and legacy account rows
        sessions/{userId}/{sessionId}.jsonl # Conversation snapshots; may include compact provider_contexts and tool_traces
    github/
      data/
        lingobridge.db                   # GitHub review sessions, sync cursors, and legacy account rows
        sessions/{reviewKey}/{sessionId}.jsonl # Synthetic review history and tool_traces
```

Each platform has its own SQLite database and data directory. The middle layer
opens a store for the selected platform and passes only that scoped store to the
platform adapter, so WeChat code cannot read Feishu data and Feishu code cannot
read WeChat data through the storage API. Account ownership is platform
specific: WeChat accounts live in SQLite because QR login returns upstream
session state, while Feishu and GitHub accounts live only under
`platforms.<platform>.accounts.<name>` in config. Deleting a Feishu or GitHub
account removes the config entry, clears that account's sync cursor, and removes
a matching legacy SQLite account row if one exists; sessions and media are left
intact because current history records are not account-id scoped.

## Internal Architecture

LingoBridge uses a multi-platform frontend, shared middle layer, and multi-provider backend structure:

```
cmd/lingobridge/            # Thin CLI entrypoint
internal/app/               # CLI dispatch, account catalog, model setup, runtime orchestration, reload wiring
internal/config/            # Shared config load/save, paths, LLM/MCP defaults/validation, platforms.<platform> YAML preservation
internal/platform/          # Platform registry and shared platform definition types
internal/platform/builtins/ # Built-in platform registry assembly
internal/platform/wechat/   # WeChat account/runtime definition and frontend adapter: native events/API <-> core messages
internal/platform/wechat/monitor/ # WeChat monitor, reply sender, and media handling
internal/platform/feishu/   # Feishu account config schema and frontend support types
internal/platform/feishu/definition/ # Feishu account/runtime definition assembly
internal/platform/feishu/monitor/ # Feishu long-connection monitor, message/text-stream adapter, and event hooks
internal/platform/feishu/tools/ # Feishu platform-level LLM tools, including Docs helpers and LiteLLM account invitations
internal/platform/github/   # GitHub account/runtime definition, App auth, PR polling, review prompt construction, and MCP review tool guards
internal/core/              # Middle layer: scoped platform config/data APIs, tool orchestration, commands, sessions, LLM orchestration
internal/tools/             # Shared tool domain interfaces and provider-neutral spec/call/result/options types
internal/mcp/               # Global MCP host/client sessions and MCP tool adapters exposed through tools.Provider
internal/store/             # Platform-scoped SQLite accounts/sessions/preferences/cursors, JSONL history, media persistence
internal/llm/               # Backend provider adapters: OpenAI-compatible and Anthropic APIs
internal/session/           # Session manager backed by the scoped store
internal/commands/          # Shared in-chat slash commands
internal/runner/            # Account supervisor and monitor lifecycle
internal/control/           # Local Unix-socket reload control API
```

In-chat slash commands live in `internal/commands/` and are shared by every
platform adapter unless that platform's command policy disables them.
Built-in platforms are assembled by `internal/platform/builtins`; platform-side
packages own account listing, creation, deletion, and runtime factories through
exported `Definition` functions. The app layer and runtime create a
`core.PlatformContext` for the active platform, and platform code uses that
context to persist its own config and data without receiving access to other
platform stores.

## Tech Stack

Go 1.25.1, SQLite, YAML, Feishu Open Platform Go SDK. Single binary, minimal dependencies.
