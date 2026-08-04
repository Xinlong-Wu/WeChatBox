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
account source used by `account list` and `run`. Re-running `account new
feishu` for the same account name updates its app credentials while preserving
the existing OAuth base URL, callback URL, and optional callback-listener
settings.

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

For Feishu, enable bot capability and configure long-connection delivery in
the Feishu Open Platform app console. Subscribe to the
`im.message.receive_v1` event. Add any extra event subscriptions, such as
`p2p_chat_create` or
`im.chat.access_event.bot_p2p_chat_entered_v1`, only when they are listed under
`platforms.feishu.events` with an explicit `version`. The first version
supports text messages in 1:1 chats and group messages that mention the bot.
When Feishu Docs tools are enabled, open **Events & Callbacks > Callback
Configuration** and add the Card action trigger callback
(`card.action.trigger`) so the same long connection can receive operation and
resource-card button clicks. This is a callback, not an event: do not add it
under `platforms.feishu.events`; LingoBridge registers its SDK handler
automatically. External resource authorization also requires the account's
public `oauth_callback_url` to be registered in the app's OAuth redirect URL
configuration.

### 4. Run

```bash
./lingobridge run
```

Listens to all enabled accounts concurrently. If no enabled accounts exist yet,
it stays running and waits for a later account reload. Use `--account` to run a
specific one, and `--verbose` to set the log level (`all`, `debug`, `info`,
`warn`, or `error`; default `info`). Use `all` to include Feishu SDK diagnostics.
Raw long-connection event/card payloads are still suppressed because they may
contain user-entered form data or callback tokens.

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
| `/current` | Show current session and that session's model |
| `/new [name]` | Create a new conversation session |
| `/list` | List your sessions |
| `/switch <name>` | Switch current session |
| `/rename <name>` | Rename current session |
| `/archive [name]` | Archive a session |
| `/clear` | Archive the current session and start a new one |
| `/model [name]` | Show or switch the current session's model profile |
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
default. A typical configuration is:

```yaml
platforms:
  feishu:
    accounts:
      fsbot:
        app_id: cli_xxx
        app_secret: your-app-secret
        base_url: https://open.feishu.cn
        oauth_base_url: https://accounts.feishu.cn
        oauth_callback_url: https://oauth.wulongxin.com/feishu/oauth/callback
        # Optional: enables direct HTTP callbacks.
        # oauth_callback_listen_address: 127.0.0.1:18080
    tools:
      docs:
        enabled: true
        allow_write: true
```

Register the exact public HTTPS `oauth_callback_url` in the Feishu app console.
With only that field configured, LingoBridge does not open an HTTP port: after
authorization, the requester copies the complete callback URL from the browser
address bar (or copies only the authorization code), returns to the original
Feishu resource card, and submits it there. The callback page does not need to
be reachable for this manual handoff, although some mobile or in-app browsers
may not preserve an unreachable URL reliably.

Optionally configure `oauth_callback_listen_address` to enable direct HTTP
callbacks. The public callback URL may be reverse-proxied to that local TCP
listener, and LingoBridge serves the path taken from the callback URL. The
resource card still accepts a manual handoff as a fallback. A listen address
without a callback URL is invalid. If neither field is configured, Bot-owned
resources and already live-verifiable grants still work, but LingoBridge cannot
start a new external resource grant. The removed `oauth_redirect_uri` and
`oauth_listen_address` names are not accepted as compatibility aliases.

When Docs is enabled, LingoBridge registers `feishu_docs_request_access`,
`feishu_docs_search`, `feishu_docs_read`, and
`feishu_docs_folder_list`. With `allow_write: true`, it also registers
`feishu_docs_folder_create`, `feishu_docs_create`, and
`feishu_docs_append`.

`feishu_docs_request_access` verifies or requests `read` or `write` access to
one exact Feishu file or folder for the trusted current chat. Its arguments are
`resource_type`, `resource_token`, optional `resource_url`, `permission`,
required `once_duration_minutes` (an integer from 10 through 60), and optional
user-visible `reason`. `once_duration_minutes` is the model's suggested
temporary window; the user can instead choose permanent authorization on the
card. `write` also satisfies `read`. The aliases
`bot_root` and `chat_default_folder` resolve to the current Bot account's root
and the current chat's default Bot folder.

Protected document and folder tools use a side-effect-free resource checker
before calling Feishu. The checker reads the trusted runtime account, actor,
and chat, then accepts Bot-owned resources or requires an active scoped local
grant plus a live Feishu capability. It never sends a card, starts OAuth,
creates a workflow, or retries the protected operation. Missing authorization
is returned to the model as a structured `resource_authorization_required`
tool error containing `required_tool`, `resource_type`, `resource_token`, and
`permission`; the model can then call `feishu_docs_request_access` explicitly
and retry the original tool after the asynchronous workflow completes.
Bot-owned resources are accepted directly from the ownership registry and do
not require a synthetic capability or grant row. Live verification may update
the local capability verification timestamp or revoke an invalid local
capability/grant, but the checker never changes Feishu collaborator permissions.

Folder and document creation automatically run the same side-effect-free
`folder/write` check on the actual parent folder before any operation approval
or Feishu create API call. Bot-owned resources pass directly. For other
resources, LingoBridge requires a reusable local grant scoped to the exact
account, requesting user, current chat, resource, and permission, plus the
separate Feishu-side capability for the exact Bot or `openchat` collaborator.
`write` satisfies `read`, while a read-only grant never satisfies a write
request. Both layers are verified against Feishu before reuse. No protected
tool accepts an `access_request_id`; when the structured authorization error is
returned, the model calls `feishu_docs_request_access` and retries the original
tool after authorization completes. Resource request IDs remain internal to
the workflow, card, OAuth, continuation, and audit records.

Docx and folder create requests use a durable `feishu_remote_operations`
ledger because the corresponding Feishu create APIs do not expose a
`client_token`. The ledger is written before the first remote call and allows
only one `prepared` caller to enter `remote_started`. A timeout, lost response,
empty success body, HTTP 408/429, or server error is never retried as another
create. LingoBridge instead lists the exact parent and conservatively matches
resource type, exact name, application owner, and a bounded creation-time
window. Only one unclaimed candidate may be adopted; zero, multiple, or
already-claimed candidates return `outcome_unknown`. Both
`feishu_docs_create` and `feishu_docs_folder_create` accept a retry containing
only the returned `request_id`; this repairs or re-runs reconciliation and
never repeats the create API after `remote_started`. If initial document text
was requested, its append uses the same durable append path described below.
Older or interrupted create requests that do not have an append ledger still
return the created document with an explicit warning instead of guessing and
appending the text again.

Docx block insertion uses a separate durable
`feishu_docx_append_operations` ledger. Before the first append mutation,
LingoBridge reads the top-level child count once, freezes the exact insertion
index, `client_token`, and serialized block request, and stores the request
envelope as account-scoped authenticated ciphertext. The logical payload hash
binds the request to the exact account, actor, chat, document, and block
content; replaying the same request ID with a different scope, document, or
content fails closed. An atomic `prepared` to `remote_started` claim allows
only one first caller across Store instances. Every first call and recovery
also requires the caller's live account runtime lease and holds a bounded,
random execution-token lease. A new account runtime owner may take over an
interrupted call, while a stale owner cannot reclaim it or persist a late
result; same-owner concurrent recoveries are rejected by CAS. Taking over
`remote_started` first promotes it to `outcome_unknown`, because the previous
owner may already have reached Feishu. In-process reconciliation and startup
recovery always decrypt and reuse the same frozen request instead of reading a
new child count. Once any response is uncertain, later rejection cannot prove
that the first request did not mutate the document, so the ledger remains
`outcome_unknown` and retains its ciphertext for safe same-token recovery.
Definite success or a definite first-call rejection clears the encrypted
envelope. Startup also reconciles direct append/create workflows when the
append ledger reached a terminal state immediately before a process crash.
Every direct append/create workflow state write uses the same atomic ledger
mapping, so a stale caller cannot overwrite a ledger-authoritative terminal
state. Legacy requests without an append ledger retain their caller-provided
state, and tool-approval workflows remain independently managed.

When a new local grant is required, LingoBridge first sends a Card V2 choice
with **允许 N 分钟**, **永久允许**, and **拒绝**. A temporary `once` grant starts
when the Feishu capability is successfully verified, remains reusable for the
selected 10–60 minute window, and then expires only in LingoBridge. An `all`
grant has no local expiry. Neither mode removes or downgrades the Bot/group
collaborator in Feishu when local authorization ends. After approval,
LingoBridge first reuses a live capability, then an encrypted persisted user
OAuth credential (refreshing it silently when needed). Only when user
reauthorization is required does it update the same request and card with a
link to Feishu's official OAuth page. The OAuth card contains a 1,000-character
`oauth_result` input and a submit button for the complete callback URL or raw
authorization code when LingoBridge has no browser-reachable callback listener.
Both card stages show a server-resolved resource name, the Feishu resource
link, requested read/write scope, exact collaborator, local `once`/`all`
semantics, and whether an existing capability, stored OAuth credential, or new
OAuth authorization will be used. Names come from the Bot ownership registry
first, then the current-chat folder/document binding, with a resource-type plus
hashed reference fallback; model input cannot replace trusted metadata. The raw
resource token is not rendered as a separate visible card field. OAuth status
inspection reads only credential metadata and does not decrypt or refresh a
token.
The whole workflow remains bound to the original Feishu user, account, chat,
card message, and global request ID; the model cannot provide another
`chat_id`.

`feishu_docs_read` and `feishu_docs_append` run the resource checker for the
resolved Docx before the Feishu API call. Bot-owned documents pass directly;
an external Docx requires the current actor/chat's `docx/read` or `docx/write`
grant and live capability. Missing access returns the structured error above
instead of silently sending a card. External documents are not permanently
bound to the chat. A Bot-owned document missing its local binding is repaired
only when its recorded parent is a fully shared Bot folder in the same chat;
Bot-owned documents from another chat are rejected even if their token is
provided.

The OAuth flow uses a cryptographically random state and Feishu's
confidential-client authorization-code flow. Its hash is the durable callback
verifier. Until the exact OAuth handoff card is confirmed delivered, the state
itself is also retained as account/actor/request-bound AES-GCM ciphertext so a
lost card-update response or restart can resend the same authorization URL.
The durable card-delivery worker retries that same state and URL with bounded
backoff until the resource-access request expires. After a successful card
update, the recovery ciphertext is cleared while the hash and delivery marker
remain until callback claim. The Feishu SDK
authenticates the token exchange with the app's configured client secret;
LingoBridge deliberately omits `code_challenge`, `code_challenge_method`, and
`code_verifier`. Feishu's authorization code is valid for five minutes and can
be exchanged only once. A complete URL submitted through the card must match
the configured callback scheme, host, and path exactly and return the
request's state. A raw code is accepted only from the original account, user,
chat, card message, and request. The HTTP and card transports atomically claim
the same one-time request, so a duplicate or racing submission cannot grant
twice. Requests generated by an earlier PKCE-enabled LingoBridge build are
rejected before token exchange and must be recreated with a new authorization
card.

LingoBridge exchanges the code for a `user_access_token`, verifies that the
authorizing user is the requester, persists the OAuth credential encrypted on
the local machine, adds the required collaborator, and then verifies the
resulting permission with the Bot tenant identity. For a non-folder document,
the collaborator is the Bot `open_id`. For an external folder in a group chat,
the collaborator is the current `openchat`; a private chat cannot directly
grant an external folder to the Bot and returns an `unsupported` result.
Feishu collaborator create/update calls do not provide an idempotency token.
Immediately before the first possible collaborator mutation, the approved
request is atomically claimed as `executing`, so concurrent callbacks cannot
issue the same mutation twice and startup will not blindly replay an uncertain
in-flight write.
If their response is lost or the operation deadline expires, LingoBridge
performs an exact live capability check, with one independent bounded retry if
that check cannot be completed, and accepts success only when the requested
collaborator permission is visible; it does not blindly replay the mutation.
If Feishu has already confirmed the collaborator permission but the local
completion transaction fails, the request remains `executing` instead of being
reported as a definite failure. Startup recovery verifies the live permission
again and completes the local capability/grant transaction without replaying
the collaborator mutation.
Authorization codes and complete callback URLs are never persisted. Access and
refresh tokens are encrypted with AES-256-GCM using an account-bound key derived
from the Feishu App Secret; they are never included in logs, model context, or
workflow continuations. Refresh responses are first encrypted with additional
data bound to the exact Bot account, OAuth actor, refresh-attempt ID, and token
field; they are re-encrypted with the normal credential context only while the
durable response is atomically applied. Rotating the App Secret makes existing
credential and staged-refresh ciphertext unreadable, causes affected
credentials to fail closed, and requires the user to complete OAuth again.
Sanitized terminal refresh attempts are retained for 30 days for bounded audit
and then deleted in batches of 500. Cleanup runs once after startup recovery and
every 24 hours while the account Runtime holds its single-active lease. Prepared
or staged attempts are never retention-deleted. A terminal row that still
contains access- or refresh-token ciphertext is also retained and logged as an
unsafe anomaly instead of being deleted.
LingoBridge updates the original card with the terminal result and does not send a separate
success/failure text message to the chat. In direct HTTP mode the browser is
additionally redirected to the Feishu resource; in manual mode the requester
stays in control of returning to the original Feishu chat. No custom H5
callback page, clipboard automation, or AppLink return is implemented.

`feishu_docs_folder_create` creates only under the Bot root or another
Bot-owned folder already bound to the current chat. It does not show a separate
operation-approval card. Its parent `folder/write` resource check runs before
creation. After creation, the Bot records the new folder as Bot-owned, grants
the current private-chat user or group `full_access`, and binds the folder to
that chat. If sharing fails after the folder exists, retry with only the
returned folder-create `request_id`; this retry ID identifies the partial
folder-create workflow and is not a resource credential.

`feishu_docs_create` creates a Docx only in a Bot-owned folder bound to the
current chat. Omitting `folder_token` selects that chat's default Bot folder.
To place a document in a non-Bot-owned target directory, use the
create-in-Bot-folder, copy-to-target, then delete-temporary-resource flow.
Successful folder and document creation is immediately recorded as Bot-owned
resource metadata.

Approval-gated tools use one shared operation-approval service. Each tool owns
its immutable tool/action policy, user-visible fields, exact resource scope,
persisted payload, whether reusable approval is supported, and the executor
that revalidates and performs the operation. The shared service exposes one
`CheckOrRequest` path and owns trusted actor/chat validation, reusable-grant
lookup, global request IDs, SQLite state transitions, card transport and
routing, continuation, terminal results, and restart reconciliation. Approval
rows persist `action_key`, `resource_type`, `resource_token`, and
`supports_all`; older rows receive empty fail-closed scope fields rather than a
compatibility authorization.

`feishu_docs_create` declares the `create` action on its exact parent folder,
while `feishu_docs_append` declares a separate `append` action on its exact
target Docx. Each call first checks the required `folder/write` or `docx/write`
resource access from trusted runtime context, then invokes the shared operation
service. Without a matching operation grant the service stores the exact
request, sends the built-in raw Feishu Card V2 form to the current chat, and
immediately returns `pending_approval` to the model. The form offers **同意一次**,
**全部同意**, and **拒绝**, plus an optional suggestion field. Tools that disable
reusable authorization do not receive an **全部同意** button, and a forged
approve-all callback is rejected. Callback values carry LingoBridge's approval
kind, approval ID, and action; the suggestion text is not persisted or written
to logs (only whether it was present and its character count may be logged). No
card ID or `template_id` configuration is required.

Only the Feishu user who triggered that LLM turn can act on the operation card.
The callback is also bound to the original bot account, chat, and card message;
the pending card expires after 10 minutes and can be consumed only once.
**同意一次** executes only that stored workflow. **全部同意** also creates or
renews a permanent local operation grant with the exact scope
`account + actor + chat + tool + action + resource`. A later call matching every
scope field bypasses the operation card, but it never bypasses the trusted
resource-access check. Create and append grants do not inherit from each other,
and a grant for one folder or document cannot authorize another resource. A
different user, bot, chat, tool, action, or resource requires a new operation
card. During schema migration, legacy 24-hour tool-wide grants are cleared
fail-closed because they cannot be safely promoted to this permanent exact
scope without a new user decision.
Permanent exact-scope operation grants are stored in
`tool_approval_grants`; `tool_operation_grants` is not a schema table.

Card-approved document creation and append run asynchronously and update the
original card with the result; they do not send a second standalone
success/failure text message. Immediately before execution, LingoBridge
reconstructs the original trusted actor/chat context and revalidates the exact
target folder or document. If permission was revoked, no create or append API
call is made. The approval callback responds within three seconds; terminal
denial/expiry states can replace the card in that response, while an approved
asynchronous operation uses the callback token and Feishu's delayed card-update
API for its final state. After Feishu creates a document, LingoBridge records
Bot ownership and the current-chat binding before appending optional initial
content. If any post-create step fails, the card and durable workflow result
report partial success and tell the model not to create a duplicate; an initial
append failure can be recovered through `feishu_docs_append`, subject to that
tool's independent operation approval. Append is restricted to a document
bound to the current trusted chat or an external Docx with a live scoped
`docx/write` grant and Feishu capability.

Pending operation and resource-access requests survive process restarts in the
Feishu platform SQLite database. The document payload is retained only while
operation authorization is pending/executing and is cleared on denial, expiry,
success, or failure. A nonterminal Docx append additionally retains only its
authenticated encrypted request envelope; terminal append states clear that
ciphertext while keeping non-sensitive hashes and frozen request metadata for
idempotency checks. Mutable runtime-owner, execution-token, and lease metadata
is kept outside the encrypted envelope and cleared when an attempt becomes
recoverable or terminal. OAuth state is verified by its hash. Before handoff
delivery is confirmed, the original state is temporarily stored only as
authenticated ciphertext; delivery success clears that ciphertext, and either
the HTTP callback or exact-context card submission clears the remaining hash.
If delivery was not confirmed, startup decrypts and resends the same state and
authorization URL through the durable card-delivery worker rather than
generating a replacement link. The
existing `pkce_verifier` storage column remains only to identify and reject
in-flight requests created by older versions; new requests leave it empty, and
any legacy value is cleared on claim. Resource-access requests also retain the
server-resolved display name so the same trusted resource context survives the
choice-card to OAuth-card transition and process recovery. Verified user OAuth
credentials are kept separately as authenticated ciphertext with their exact Feishu response
expiries, scopes, authorization time, refresh version, and mandatory
reauthorization time. Access tokens are refreshed within five minutes of
expiry. Before calling Feishu, LingoBridge creates a leased
`feishu_oauth_refresh_attempts` row in `prepared` state using the credential
version as a compare-and-swap boundary, so concurrent Store instances and
processes do not consume the same one-time refresh token twice. Initial OAuth
credential replacement and refresh-attempt transitions both serialize their
SQLite write transactions before reading credential state. A successful
response is encrypted and
persisted as `response_staged` before the credential row is changed; the final
transaction re-encrypts and replaces both tokens, increments the credential
version, clears staged ciphertext, and marks the attempt `completed`. Competing
callers briefly wait and reload the credential instead of calling Feishu again.
At startup, a `response_staged` attempt is completed without replaying the
refresh API. An expired `prepared` attempt has an unknown remote outcome, so it
is marked `ambiguous` and the credential requires OAuth again. Feishu provides
no idempotency key for refresh: if the process stops after Feishu returns new
tokens but before `response_staged` is committed, those response values cannot
be recovered and LingoBridge deliberately fails closed. A revoked, expired, or
already-consumed refresh token also requires OAuth again, as does the mandatory
365-day reauthorization boundary. Operations interrupted while already
executing are marked failed rather than retried automatically, avoiding
duplicate creation.
An approved request that is waiting on an OAuth browser handoff keeps its
one-time state hash across restart. A delivered authorization URL remains
usable; an unconfirmed delivery is retried with the same encrypted state and
same URL. Startup does not invalidate or replace a still-pending handoff.
Feishu resource capabilities store the exact collaborator subject, actual
read/write permission, source OAuth actor, source request, live-verification
time, and active/revoked state. Local resource grants separately store the exact
requesting actor, current chat, resource, permission, `once`/`all` mode, local
expiry, source request, and state. Read and write grants use separate rows so a
permanent read authorization can coexist with a temporary write authorization.
Expired `once` grants only stop future LingoBridge operations; they do not
remove the Bot or group collaborator in Feishu. Neither layer contains a user
OAuth token, and both must be active before reuse. Resource request IDs remain
workflow and audit identifiers; the reusable authority is the scoped local
grant plus the live Feishu capability, not one-time consumption of a request.

The following matrix lists the narrow Feishu scopes used by the current API
calls. Tenant scopes are used with the Bot application's
`tenant_access_token`. User OAuth scopes must also be enabled for the app and
published, but LingoBridge requests them on `user_access_token` only when an
external resource needs a new collaborator or collaborator upgrade.

| LingoBridge path | Feishu API use | Tenant app scope | User OAuth scope |
| --- | --- | --- | --- |
| Bot replies, cards, message/card updates | Send/reply/update messages and delayed Card V2 updates | `im:message:send_as_bot` | — |
| `feishu_chat_history_get` in a private chat | List messages for the trusted p2p `chat_id` | `im:message:readonly` | — |
| `feishu_chat_history_get` in a group | List messages for the trusted group `chat_id` | `im:message:readonly` plus `im:message.group_msg` | — |
| `feishu_docs_search` | Search documents and Wiki pages inside bound folders | `search:docs:read` | — |
| `feishu_docs_read` | Read Docx raw text | `docx:document:readonly` | — |
| `feishu_docs_folder_list` | Read LingoBridge's local chat-folder bindings | No additional Feishu API scope | — |
| External document live verification | Check whether the Bot can view/edit the exact resource | `docs:permission.member:auth` | — |
| External folder live verification | List the exact folder's collaborators | `docs:permission.member:retrieve` | — |
| External resource authorization | Identify the authorizing user, add a collaborator, or upgrade it from read to write | — | `auth:user.id:read`, `docs:permission.member:create`, `docs:permission.member:update`, `offline_access` |
| `feishu_docs_folder_create` | Read Bot root metadata, create a Bot folder, and share it with the current user/group | `drive:drive.metadata:readonly`, `space:folder:create`, `docs:permission.member:create` | — |
| `feishu_docs_create` without initial content | Create a Docx in a Bot-owned folder | `docx:document:create` | — |
| `feishu_docs_create` with initial content and `feishu_docs_append` | Read the current top-level child count, then create Docx child blocks | `docx:document:readonly`, `docx:document:write_only` | — |

The broader `im:message`, `drive:drive`, and `docx:document` permissions can
replace their narrower alternatives where the corresponding official API
lists them. The Card action trigger callback itself has no API-scope
requirement. `contact:user.employee_id:readonly` is not required because the
current implementation does not request `user_id`-typed response fields from
the Docx APIs.

The official OAuth page requests the user scopes `auth:user.id:read`,
`docs:permission.member:create`, `docs:permission.member:update`, and
`offline_access`. Apply those permissions in the app console, enable the
setting that permits refreshing
`user_access_token`, publish a new app version, and obtain tenant administrator
approval before use. Token lifetimes always come from Feishu's token response;
LingoBridge does not hard-code the access- or refresh-token expiry. The
resource grant choice and any required OAuth handoff share one durable request
and card, while operation approval is a separate workflow. All durable
workflows use one global `request_id` namespace. A stored user credential that
lacks a currently required scope is invalidated and the same resource request
switches back to OAuth rather than repeatedly attempting an under-scoped API
call.

The card and document integration is aligned with Feishu's official
[AI-friendly documentation index](https://open.feishu.cn/llms.txt), including
[Card action callbacks](https://open.feishu.cn/document/feishu-cards/card-callback-communication.md),
[delayed card updates](https://open.feishu.cn/document/server-docs/im-v1/message-card/delay-update-message-card.md),
[OAuth authorization codes](https://open.feishu.cn/document/authentication-management/access-token/obtain-oauth-code.md),
[permission checks](https://open.feishu.cn/document/server-docs/docs/permission/permission-member/auth.md),
[listing collaborators](https://open.feishu.cn/document/server-docs/docs/permission/permission-member/list.md),
[adding collaborators](https://open.feishu.cn/document/server-docs/docs/permission/permission-member/create.md),
[updating collaborators](https://open.feishu.cn/document/server-docs/docs/permission/permission-member/update.md),
[Create folder](https://open.feishu.cn/document/server-docs/docs/drive-v1/folder/create_folder.md),
[Create document](https://open.feishu.cn/document/server-docs/docs/docs/docx-v1/document/create.md),
[searching documents and Wiki](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/search-v2/doc_wiki/search.md),
[reading Docx raw content](https://open.feishu.cn/document/ukTMukTMukTM/uUDN04SN0QjL1QDN/document-docx/docx-v1/document/raw_content.md),
[reading Docx child blocks](https://open.feishu.cn/document/ukTMukTMukTM/uUDN04SN0QjL1QDN/document-docx/docx-v1/document-block-children/get.md),
[creating Docx child blocks](https://open.feishu.cn/document/ukTMukTMukTM/uUDN04SN0QjL1QDN/document-docx/docx-v1/document-block-children/create.md),
and [listing chat history](https://open.feishu.cn/document/server-docs/im-v1/message/list.md).

Feishu can also expose `feishu_chat_history_get` to read recent messages from
the Feishu chat that triggered the current LLM turn. Enable it under
`platforms.feishu.tools.chat_history`. The runtime binds the trusted current
`chat_id`; the model cannot provide or switch to another chat ID. The optional
`limit` argument defaults to 20 and is capped at 100. Results are fetched from
the Feishu message history API, returned in chronological order, and bounded by
`platforms.feishu.tools.max_chars`. Text and rich-text post messages are
rendered as readable content; other message types use safe placeholders rather
than exposing file or image keys.

Chat history is disabled by default. This implementation uses the Bot's
`tenant_access_token`, so the recommended narrow p2p scope is
`im:message:readonly`; the broader `im:message` or historical
`im:message.history:readonly` scope is also accepted by Feishu. Group history
additionally requires `im:message.group_msg` (`获取群组中所有消息`), and the Bot
must be a member of that group. The user-identity-only
`im:message.p2p_msg:get_as_user` and `im:message.group_msg:get_as_user` scopes do
not apply to this tenant-identity implementation. API or permission failures
are returned to the model as tool errors with an actionable group-permission
hint.

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

All automated reviews and comment-triggered interactions for the same PR share
one conversation session keyed by the base repository and PR number. The
session is preserved when the PR head SHA changes. A human comment beginning
with `/review` triggers another review, while `/bot <message>` continues the
same PR conversation and posts the response back to the issue or review-comment
thread. Different PRs use separate sessions.

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
| `platforms.feishu.accounts.<name>.oauth_base_url` | `https://accounts.feishu.cn` for the default Feishu API domain | Feishu OAuth authorization/token service base URL |
| `platforms.feishu.accounts.<name>.oauth_callback_url` | — | Public absolute OAuth callback URL registered exactly in the Feishu app console. By itself it enables listener-free card handoff |
| `platforms.feishu.accounts.<name>.oauth_callback_listen_address` | — | Optional local TCP address for direct OAuth HTTP callbacks, such as `127.0.0.1:18080`; requires `oauth_callback_url` |
| `platforms.feishu.events[].name` | — | Extra Feishu event to register; v1.0 supports customized event names such as `p2p_chat_create`, and v2.0 currently supports `im.chat.access_event.bot_p2p_chat_entered_v1`. Built-in `im.message.receive_v1` and `card.action.trigger` handlers must not be configured here |
| `platforms.feishu.events[].version` | — | Required Feishu event protocol version: `"1.0"` uses `OnCustomizedEvent`; `"2.0"` uses a built-in `OnP2...` mapping |
| `platforms.feishu.events[].run` | — | Shell command string or list of shell command strings to run for the event |
| `platforms.feishu.tools.max_results` | `5` | Shared maximum result count for Feishu tools that return lists, including `feishu_docs_search` |
| `platforms.feishu.tools.max_chars` | `12000` | Shared maximum character count for Feishu tools that return content, including `feishu_chat_history_get` and `feishu_docs_read` |
| `platforms.feishu.tools.chat_history.enabled` | `false` | Enable `feishu_chat_history_get` for the current trusted Feishu `chat_id`; each call returns at most 100 messages |
| `platforms.feishu.tools.docs.enabled` | `false` | Enable Feishu Docs tools for tool-capable LLM profiles |
| `platforms.feishu.tools.docs.allow_write` | `false` | Register folder/document create and append tools when both resource access and operation approval services are available. Protected tools check the trusted actor/chat's scoped resource grant directly and never accept an access request ID. Document create and append each require requester-only operation approval unless a permanent grant matches the exact account, actor, chat, tool, action, and resource |
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
Model selection is stored independently for each conversation session. New
sessions use `llm.default_model` until `/model <name>` changes that session.
On startup and reload, `run` validates the default model profile and resets any
saved per-session model preference that no longer exists back to
`llm.default_model`.

## Storage

```
~/.lingobridge/
  config.yaml                          # Shared LLM/MCP config and platform-private platforms.<platform> config
  lingobridge.sock                       # Local control socket used by a running process
  platforms/
    wechat/
      data/
        lingobridge.db                   # WeChat accounts, sessions with model preferences, current-session pointers, sync cursors
        sessions/{userId}/{sessionId}.jsonl # Revisioned conversation snapshots with compact provider_contexts and tool_traces
        media/{safeUserId}/{safeSessionId}/
    feishu/
      data/
        lingobridge.db                   # Feishu sessions, workflow requests/results/continuations, chat-bound Docs metadata, Bot resources, resource grants, operation approvals, and permanent exact-scope operation grants
        sessions/{userId}/{sessionId}.jsonl # Revisioned conversation snapshots with compact provider_contexts and tool_traces
    github/
      data/
        lingobridge.db                   # GitHub review sessions, sync cursors, and legacy account rows
        sessions/{reviewKey}/{sessionId}.jsonl # Revisioned synthetic review history and tool_traces
```

Each platform has its own SQLite database and data directory. The middle layer
opens a store for the selected platform and passes only that scoped store to the
platform adapter, so WeChat code cannot read Feishu data and Feishu code cannot
read WeChat data through the storage API. Account ownership is platform
specific: WeChat accounts live in SQLite because QR login returns upstream
session state, while Feishu and GitHub accounts live only under
`platforms.<platform>.accounts.<name>` in config. Deleting a Feishu or GitHub
account removes the config entry and clears that account's sync cursor. Feishu
account deletion also removes its chat-bound document/folder metadata,
Bot-resource records, Feishu-side resource capabilities, local resource-access
requests/grants, encrypted user OAuth credentials and their refresh-attempt
records, account runtime lease, pending/completed tool approvals, reusable
approval grants, pending/completed card-delivery outbox rows, durable remote
create-operation rows, and their global workflow request rows.
Sessions and media are left intact because current history records are not
account-id scoped.

Each SQLite `sessions` row owns its `model_name`. During the schema migration
from older databases, the former user-level model preference is copied to all
existing sessions for that user and the legacy preference column is removed.
Each JSONL conversation snapshot also carries a monotonic `revision`. Saves use
compare-and-swap against the revision loaded at turn start, so a stale turn
cannot replace newer history. Legacy snapshots without the field load as
revision zero and receive revision one on their next successful save. Store
instances that share the same platform database serialize JSONL CAS/truncate
operations through SQLite-backed conversation file locks and use a unique
temporary file per write, so concurrent processes cannot both commit the same
revision or overwrite one another's staging file.

Asynchronous approval and authorization workflows persist one sanitized
terminal result per global request ID plus a continuation bound to the trusted
account, user, session, chat type, actor, source message, origin turn, and tool
call.
The origin conversation revision is committed only after its CAS save. A
terminal result can arrive before or after that commit; the continuation becomes
resumable only when both exist. Ready work is leased while processing, and an
expired lease can be reclaimed after restart. Continuations do not store model
names, OAuth codes, callback URLs, access tokens, refresh tokens, or card callback
tokens. Deleting a Feishu account's Docs or approval data also deletes the
matching workflow results and continuations.

The same JSONL CAS stores a model-invisible origin receipt containing only the
workflow request ID, tool call/name, and committed revision. Origin recovery
checks that receipt before scanning ordinary messages, so later conversation
compaction cannot erase the proof that the pending workflow turn committed.
Recovery advances through uncommitted origins with a keyset cursor; an old row
whose conversation proof is unavailable cannot permanently starve newer rows.

Operation approvals and resource OAuth callbacks persist a model-safe terminal
payload for success, warning, denial, expiry, and failure. The original card is
the immediate user-visible status surface; no duplicate result text is sent.
At startup, LingoBridge also reconciles terminal approval/resource records that
are missing a result because the process stopped between the workflow-specific
state update and result insertion. Recovered operation results deliberately do
not guess or replay a possibly completed remote write.

Card updates use a durable `feishu_card_deliveries` outbox. The business-state
transition and its delivery row are committed in the same SQLite transaction
for OAuth handoff, operation-approval terminal state, resource-access terminal
state, and continuation terminal notices. A callback token is only a synchronous
fast path for the callback currently being handled; it is never persisted.
Failed or interrupted updates are rebuilt from durable request/result state and
retried by message ID with leased claims, bounded backoff, and expired-lease
takeover. Newer card revisions supersede older revisions so a delayed OAuth or
pending update cannot overwrite a terminal result. OAuth handoff delivery stops
when the resource request expires; terminal updates have a 24-hour retry window.
The outbox stores routing, revision, lease, and retry metadata only—never card
JSON, authorization codes, callback URLs, callback tokens, or OAuth credentials.

Each Feishu account runs a continuation worker while Docs tools are enabled.
The worker scans ready work and expired processing leases, claims one durable
lease, and enters the exact original session lane without waiting for another
user message. It loads that session's current model selection and latest
conversation revision at execution time, so
changing models while a card is pending affects the resumed turn without moving
the workflow to another session. An archived/deleted target session cancels the
continuation; transient model, tool, storage, or delivery failures use bounded
backoff retries and eventually become failed.
Runtime shutdown closes callback task admission before any new approval or
OAuth one-shot state is consumed, waits for already admitted approval/OAuth
tasks to exit, and returns an interrupted continuation lease to `ready` without
charging an attempt. Results that become ready while the worker is stopping are
picked up after restart.

Each Feishu Bot account also uses a durable account-level runtime lease. Only
one active LingoBridge runtime may start recovery, workers, callbacks, and the
long connection for the same `account_id`; a second runtime exits before it can
change workflow state. The active runtime renews a 30-second lease every 10
seconds and keeps renewing while admitted background work drains during normal
shutdown. After a crash, another runtime can take over after the lease expires,
and an old owner cannot renew or release the replacement's lease. Competing
processes or machines must open the same Feishu SQLite database for this
single-active guarantee; separate local database files cannot coordinate the
account lease. Normal lifecycle cancellation is deliberately distinct from
lease ownership loss: orderly shutdown keeps ownership-bound admitted work
alive until it drains, while a lost lease cancels approval/resource side-effect
contexts and leaves their durable `executing` state for the replacement runtime
to recover. This narrows the stale-owner overlap window; remote APIs that have
already accepted a request still rely on their operation ledger, stable
idempotency token, or conservative live verification for final reconciliation.
Independent same-token append reconciliation is also bounded by this ownership
context: normal shutdown may finish an already admitted retry, but a lost
account lease prevents the stale runtime from issuing the reconciliation call.

The resumed turn receives a runtime-generated workflow-result event with a
per-turn attestation in the system prompt. The event payload is treated as data,
not instructions, and ordinary user text cannot set its internal JSONL marker.
The normal tool loop remains available, so a successful resource grant can
continue the original operation; a completed side-effecting operation is not
executed again. The resumed user/assistant pair is saved through the normal
conversation CAS before any reply is sent. Its internal event ID, exact
committed revision, and any newly pending workflow IDs are retained in JSONL,
allowing a retry to reconcile a chained workflow against the revision that
created it and replay the already committed assistant response without calling
the model again. Automatic resume suppresses compaction progress notices so the
persisted assistant chunks keep the same deterministic Feishu message UUID
positions across partial delivery retries.

Docx append resolves every top-level child page before writing and fails closed
when the position response is incomplete or inconsistent. Its block-create
request uses a deterministic Feishu `client_token` derived from the durable
global workflow request ID. An uncertain transport/server response gets one
independent bounded replay with the exact same token and request body, so
reconciling that same logical append does not add the block batch twice. If the
replay response is also lost, the workflow becomes `partial`/`outcome_unknown`
instead of a definite failure: the card or direct tool result tells the caller
to inspect the document and not repeat the append. The same conservative
message is used when a newly created document's initial-content append cannot
be confirmed.

An asynchronous tool returns its request ID to core as runtime-only result
metadata; that ID is not part of the model-visible tool JSON. The Feishu manager
creates the waiting continuation from the trusted execution context after the
card is durably bound. Core commits the continuation to the new conversation
revision only after `SaveHistoryCAS` succeeds, and cancels it when the tool loop,
assistant conversion, or conversation save fails.

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
internal/platform/feishu/monitor/ # Feishu long-connection monitor, message/text-stream adapter, cards/callback routing, resource workflow/recovery, OAuth credentials, permission guard, approvals, and event hooks
internal/platform/feishu/idempotency/ # Deterministic, namespaced Feishu idempotency-key helpers
internal/platform/feishu/secure/ # Shared account-scoped authenticated-encryption primitive for Feishu sensitive state
internal/platform/feishu/tools/ # Feishu platform-level LLM tool adapters and shared Docs/folder services, authorization contracts, durable append/create recovery, and LiteLLM invitations
internal/platform/github/   # GitHub account/runtime definition, App auth, PR polling, review prompt construction, and MCP review tool guards
internal/core/              # Middle layer: scoped platform config/data APIs, tool orchestration, commands, sessions, LLM orchestration
internal/tools/             # Shared tool interfaces, provider-neutral spec/call/result types, and runtime-owned execution context
internal/mcp/               # Global MCP host/client sessions and MCP tool adapters exposed through tools.Provider
internal/store/             # Platform-scoped SQLite accounts/sessions/preferences/cursors/tool approvals, resource capabilities/grants, encrypted Feishu OAuth credentials, durable refresh/create/append operations, JSONL history, and media persistence
internal/llm/               # Backend provider adapters: OpenAI-compatible and Anthropic APIs
internal/session/           # Session manager backed by the scoped store
internal/commands/          # Shared in-chat slash commands
internal/runner/            # Account supervisor and monitor lifecycle
internal/control/           # Local Unix-socket reload control API
```

Within the Feishu integration, durable responsibilities are split at service
boundaries rather than implemented by each tool adapter. Resource permission
checks, startup recovery, OAuth credential/refresh rotation, card-delivery
retry, and remote-create reconciliation each have one implementation boundary.
The document and folder tool values retain only their tool name/schema and
delegate to one shared service per registered tool set. Store dependencies are
expressed as small capability interfaces for those services; the resource
access manager remains the live workflow and callback facade.

In-chat slash commands live in `internal/commands/` and are shared by every
platform adapter unless that platform's command policy disables them.
Built-in platforms are assembled by `internal/platform/builtins`; platform-side
packages own account listing, creation, deletion, and runtime factories through
exported `Definition` functions. The app layer and runtime create a
`core.PlatformContext` for the active platform, and platform code uses that
context to persist its own config and data without receiving access to other
platform stores.

Tool arguments contain only model-visible parameters. Platform adapters attach
trusted account, chat, source-message, and actor identity through Go context;
the core then binds the exact user, session, turn, provider call ID, and resolved
tool name immediately before execution. These runtime fields are not part of a
tool's JSON Schema and cannot be supplied or overridden by model arguments.
Core user turns, manual compaction, and current-session mutation commands run
through a lane keyed by platform, account, user, and session. Work for one
session is serialized while different sessions can proceed independently; the
loaded conversation revision is included in the trusted tool context.

## Tech Stack

Go 1.25.1, SQLite, YAML, Feishu Open Platform Go SDK. Single binary, minimal dependencies.
