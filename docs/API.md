# API guide

The [OpenAPI 3.1 document](openapi.yaml) describes all registered HTTP operations.
This guide explains authentication, common requests, streaming, and which
listener serves each interface. The schema version is not a claim of a stable
released SDK or full OpenAI API compatibility.

## Interfaces and authentication

| Interface | Address | Authentication |
|---|---|---|
| Owner API, `/api/v1` | App HTTP listener, default `http://127.0.0.1:7070` | `woolwire_session` cookie, except the setup exchange |
| Compatible client API, `/v1` | Same app HTTP listener | Local API bearer token, always required |
| Peer API, `/peer/v1` | Tailcat protocol port `4242` by default | TLS 1.3 device certificate, admitted signed room membership |
| Bootstrap, `/bootstrap/v1` | Creator's Tailcat port `4243` by default | Invitation admission secret, authority-pinned TLS, joining device certificate |
| Runner API, `/runner/v1` | Internal controller, default `http://woolwire-runner:8080` in Compose | Runner bearer token, including health requests |

Peer, bootstrap, and runner routes are **not** mounted on the owner HTTP
listener. A local owner cookie cannot authenticate to a peer, and room membership
does not grant access to another installation's settings.

All state-changing requests on the app listener, including setup and compatible
chat completions, must carry **`X-Woolwire-Request: 1`** or an Origin whose host
matches the request Host. Send `Content-Type: application/json` with JSON bodies.
There is no permissive cross-origin browser API. Creator administration and
moderation additionally require the local installation's creator authority.

## Pair an API session

These examples use **Bash**, `curl`, Python 3, and `jq`. Run against an installation
you own. Use a fresh setup secret from first boot or Connect Device; exchanging
it here consumes it, so it cannot also be used for another browser login.

```bash
WOOLWIRE_URL=http://127.0.0.1:7070
umask 077
WOOLWIRE_COOKIE_JAR=$(mktemp /tmp/woolwire.XXXXXX.cookies)
read -r -s -p 'One-time setup secret: ' WOOLWIRE_PAIRING_SECRET
printf '\n'
printf '%s' "$WOOLWIRE_PAIRING_SECRET" |
  python3 -c 'import json,sys; print(json.dumps({"token":sys.stdin.read()}))' |
  curl --fail-with-body -sS -c "$WOOLWIRE_COOKIE_JAR" \
    -H 'X-Woolwire-Request: 1' -H 'Content-Type: application/json' \
    --data-binary @- "$WOOLWIRE_URL/api/v1/setup"
unset WOOLWIRE_PAIRING_SECRET

curl --fail-with-body -sS -b "$WOOLWIRE_COOKIE_JAR" \
  "$WOOLWIRE_URL/api/v1/state" | jq .
```

The cookie jar is an owner credential. Keep it private and delete it when done.
The cookie is HttpOnly and SameSite=Strict for browsers. A setup reset issues a
new pairing secret; it does not invalidate existing owner sessions.

## Configure your profile and room

```bash
curl --fail-with-body -sS -b "$WOOLWIRE_COOKIE_JAR" \
  -H 'X-Woolwire-Request: 1' -H 'Content-Type: application/json' \
  -d '{"display_name":"Juniper"}' "$WOOLWIRE_URL/api/v1/profile"

curl --fail-with-body -sS -b "$WOOLWIRE_COOKIE_JAR" \
  -H 'X-Woolwire-Request: 1' -H 'Content-Type: application/json' \
  -d '{"room_name":"The Sunday Club"}' "$WOOLWIRE_URL/api/v1/room/host"
```

The host response contains the room invitation; share it privately. To join
instead, `POST /api/v1/room/join` with `{"invitation_code":"..."}`. Approval mode
may require retrying after the creator admits the device. Room creation and
joining apply to the one room supported by this installation.

## Register and discover an external model

Use a server-root URL such as `http://127.0.0.1:8081`, reachable **from the app**.
The current adapter appends `/v1/chat/completions` for inference unless the URL
already ends in `/chat/completions`; a base ending only in `/v1` would be doubled.
Discovery also tries model-list endpoints relative to the configured root.

```bash
curl --fail-with-body -sS -b "$WOOLWIRE_COOKIE_JAR" \
  -H 'X-Woolwire-Request: 1' -H 'Content-Type: application/json' \
  -d '{"endpoint_url":"http://127.0.0.1:8081","allow_private_network":true}' \
  "$WOOLWIRE_URL/api/v1/hosted-models/discover" | jq .

# Replace example-model with an identifier returned by the backend.
curl --fail-with-body -sS -b "$WOOLWIRE_COOKIE_JAR" \
  -H 'X-Woolwire-Request: 1' -H 'Content-Type: application/json' \
  -d '{"name":"My local model","backend_model":"example-model","endpoint_url":"http://127.0.0.1:8081","model_type":"external","context_limit":4096,"max_tokens":1024,"enabled":true,"published":true,"allow_private_network":true}' \
  "$WOOLWIRE_URL/api/v1/hosted-models"

curl --fail-with-body -sS -b "$WOOLWIRE_COOKIE_JAR" \
  "$WOOLWIRE_URL/api/v1/catalog" | jq .
```

`api_key` is optional for a backend that requires its own bearer authentication.
Owner hosted-model records can contain that key; do not publish their responses.
Room advertisements omit endpoint URLs and keys. Updating a hosted model is a
whole-record save, not PATCH: send the existing ID and all desired settings,
including credentials and publication state.

## Compatible text completions

Obtain the local API token in Settings or from your authenticated session:

```bash
WOOLWIRE_API_TOKEN=$(curl --fail-with-body -sS -b "$WOOLWIRE_COOKIE_JAR" \
  "$WOOLWIRE_URL/api/v1/local-api-token" | jq -r '.token')

curl --fail-with-body -sS -H "Authorization: Bearer $WOOLWIRE_API_TOKEN" \
  "$WOOLWIRE_URL/v1/models" | jq .
```

Copy an available model's `id` from that list. It is an opaque Woolwire model ID;
prefer it over the display name, which can match multiple hosts.

```bash
WOOLWIRE_MODEL_ID=model-replace-with-an-id-from-the-list
jq -n --arg model "$WOOLWIRE_MODEL_ID" \
  '{model:$model,messages:[{role:"user",content:"Give me three names for a tiny robot."}],stream:true}' |
  curl --fail-with-body -sS -N \
    -H "Authorization: Bearer $WOOLWIRE_API_TOKEN" \
    -H 'X-Woolwire-Request: 1' -H 'Content-Type: application/json' \
    --data-binary @- "$WOOLWIRE_URL/v1/chat/completions"
```

Set `stream:false` for a single completion object. The request surface is
`model`, `messages` (string `role` and string `content`), and `stream`. Unknown
JSON fields are not a supported extension mechanism. Per-request samplers,
tool calls, image arrays, embeddings, Responses, files, and audio APIs are not
implemented. Output limits come from the host's model configuration rather
than a client `max_tokens` field. Requests on this interface are not saved as
My Chats conversations.

Clients must allow the custom CSRF header in addition to the bearer token. The
usual base URL for such clients is `$WOOLWIRE_URL/v1`; this client-facing URL is
different from the **external backend root URL** configured in Settings.

## Saved chats and SSE

`POST /api/v1/chats` accepts `{"title":"Ideas","no_save":false}` and returns a
conversation record. Conversation and message records currently use Go-style
keys such as `ID`, `Title`, `NoSave`, `Role`, and `Content`; request bodies use
snake_case. Do not infer casing from another endpoint.

Send `POST /api/v1/chats/{id}/message` with:

```json
{"content":"Hello","host_member_id":"member-from-catalog","model_id":"model-from-catalog"}
```

The reply uses `text/event-stream`. A typical successful local chat stream is:

```text
data: {"delta":"Hello","request_id":"req-example"}

event: stats
data: {"request_id":"req-example","ttft_ms":120,"total_ms":500,"completion_tokens":1,"tokens_per_second":2.6}

data: [DONE]

```

Statistics are diagnostic estimates and may be absent or differ for peer paths.
SSE streams also use keepalive comments (such as `: ping`) and named `error`
events with `{"error":"..."}`. Parse blank-line-delimited events, tolerate
comments/unknown event names, and retain incomplete data across network chunks.
For compatible completions, data contains `chat.completion.chunk` objects with
`choices[].delta.content`, not the local chat's top-level `delta` field.

Errors can occur after headers have been sent. HTTP 200 alone does not prove
successful completion; an error or disconnected stream without `[DONE]` is
not a completed answer. Capture `request_id` from chat frames and send it with
`host_member_id` to `POST /api/v1/chats/{id}/cancel` to request cancellation.

No-save chats still have conversation metadata; transcript bodies stay in
memory. Applications needing durable chat history should use saved chats or
manage their own retention explicitly.

## Managed models and operations

- `GET /api/v1/managed-models/artifacts` lists discovered/downloaded weights.
  Use the returned relative **`path`** as the load `filename`.
- `GET /api/v1/managed-models/storage` reports configured status, usage, budget,
  and read-only status. Usage is zero if scanning fails; it is not a free-space
  guarantee.
- `GET /api/v1/managed-models/runner-flags` lists the llama.cpp tuning switches
  the runner accepts, each with whether it takes a value. It is generated from
  the validator's own tables, so it cannot drift from what a load will allow.
- `POST /api/v1/managed-models/storage` sets `budget_bytes`, the ceiling on
  downloaded weights. It is persisted and applied immediately; a budget below
  what is installed blocks new downloads rather than deleting anything. The
  default is 50 GB.
- `GET /api/v1/managed-models/huggingface?repo=org/repository` resolves public
  repository weight choices. The current route exposes only the repo query.
- `POST /api/v1/managed-models/download` starts a job. Poll `/downloads` and use
  `/downloads/{id}/cancel` to cancel. Downloads do not load a model automatically.
- `POST /api/v1/managed-models/prepare` needs `model_id` and `filename`; optional
  `name`, `context_limit`, `max_tokens`, `threads`, `gpu_layers`, `extra_args`, and
  `published` control registration. It saves settings without loading weights.
  Published models appear as `unloaded` and load when a request reaches the host.
- `POST /api/v1/managed-models/load` accepts the same fields and also loads the
  weights immediately. New models require `published: true` to opt into sharing;
  omitting it on an existing model preserves its sharing choice.
- `POST /api/v1/managed-models/unload` frees the runner's loaded model while
  preserving its on-demand availability. Disable or unpublish it to stop sharing.
- Artifact deletion uses a trailing wildcard. Escape each path segment, preserving
  `/` separators. Discovered shared-cache files are refused with 403.
- `GET /api/v1/metrics` is owner-authenticated **JSON**, not Prometheus exposition.
- `POST /api/v1/backup/export` accepts a passphrase and returns a server-side file
  path, not the file itself. See [Backup and restore](BACKUP_RESTORE.md).

## Errors and compatibility

Most unsuccessful local responses use a plain-text body from Go's HTTP error
helper. Do not assume every error is JSON or has the OpenAI error envelope.
Typical codes include `400` for invalid input, `401` for authentication, `403`
for Host/CSRF/authority/destination refusals, `404` for missing records/models,
`410` for a used setup secret, `429` for setup throttling, and `502`/`503` for
backend or reachability failures. Some operation-level results, such as an
endpoint test, instead return HTTP 200 with `ok:false` in JSON.

When finished with the shell examples:

```bash
rm -f "$WOOLWIRE_COOKIE_JAR"
unset WOOLWIRE_COOKIE_JAR WOOLWIRE_API_TOKEN WOOLWIRE_MODEL_ID WOOLWIRE_URL
```

## Endpoint inventory

Every explicitly registered operation is listed below. `GET` registrations also
have Go HTTP ServeMux's implicit HEAD handling. The web-asset fallback `/` is
not part of this JSON/protocol inventory. Refer to OpenAPI for request bodies,
parameters, security overrides, and individual responses.

<!-- endpoint-inventory:start -->

### Owner API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/setup` | Exchange the one-time setup secret for a session cookie |
| `GET` | `/api/v1/setup/info` | Report whether an unredeemed setup secret is outstanding |
| `POST` | `/api/v1/setup/token` | Mint a new one-time setup secret |
| `GET` | `/api/v1/local-api-token` | Read the bearer token for the OpenAI-compatible routes |
| `POST` | `/api/v1/local-api-token/regenerate` | Rotate the OpenAI-compatible bearer token |
| `GET` | `/api/v1/state` | Get local installation and room state |
| `POST` | `/api/v1/profile` | Update the display name |
| `POST` | `/api/v1/room/host` | Create a room |
| `POST` | `/api/v1/room/join` | Join a room using an invitation code |
| `POST` | `/api/v1/room/leave` | Leave the current room |
| `POST` | `/api/v1/room-admin/invitation/rotate` | Rotate the invitation code |
| `GET` | `/api/v1/room-admin/members` | List room members |
| `GET` | `/api/v1/room-admin/pending` | List devices waiting for approval |
| `POST` | `/api/v1/room-admin/members/{id}/approve` | Admit a pending device |
| `POST` | `/api/v1/room-admin/members/{id}/remove` | Remove a member |
| `POST` | `/api/v1/room-admin/approval-mode` | Require creator approval for new devices |
| `GET` | `/api/v1/hosted-models` | List locally hosted models |
| `POST` | `/api/v1/hosted-models` | Create or update a hosted model |
| `DELETE` | `/api/v1/hosted-models/{id}` | Delete a hosted model |
| `POST` | `/api/v1/hosted-models/test` | Test reachability of a model endpoint |
| `POST` | `/api/v1/hosted-models/discover` | List the models an endpoint offers |
| `GET` | `/api/v1/host-limits` | Read the fair-queue limits |
| `POST` | `/api/v1/host-limits` | Update the fair-queue limits |
| `GET` | `/api/v1/hardware` | Detected CPU, memory, and GPU profile |
| `GET` | `/api/v1/catalog` | Models offered by this node and its peers |
| `GET` | `/api/v1/managed-models/artifacts` | List available GGUF models |
| `GET` | `/api/v1/managed-models/storage` | Read model storage usage, budget, and read-only status |
| `POST` | `/api/v1/managed-models/storage` | Change the model storage budget |
| `GET` | `/api/v1/managed-models/runner-flags` | List the llama.cpp tuning flags the runner accepts |
| `DELETE` | `/api/v1/managed-models/artifacts/{filename}` | Delete a downloaded weight file |
| `POST` | `/api/v1/managed-models/download` | Start a background weight download |
| `GET` | `/api/v1/managed-models/huggingface` | List the GGUF weights a Hugging Face repository publishes |
| `GET` | `/api/v1/managed-models/downloads` | Download job status |
| `POST` | `/api/v1/managed-models/downloads/{id}/cancel` | Cancel a download job |
| `GET` | `/api/v1/managed-models/runner-health` | Runner companion status |
| `POST` | `/api/v1/managed-models/prepare` | Prepare downloaded weights for on-demand hosting |
| `POST` | `/api/v1/managed-models/load` | Load weights into the runner and advertise them |
| `POST` | `/api/v1/managed-models/unload` | Unload weights while retaining on-demand availability |
| `GET` | `/api/v1/chats` | List conversations |
| `POST` | `/api/v1/chats` | Create a conversation |
| `GET` | `/api/v1/chats/{id}` | Read a conversation and its messages |
| `DELETE` | `/api/v1/chats/{id}` | Delete a conversation |
| `POST` | `/api/v1/chats/{id}/message` | Send a message and stream the reply |
| `POST` | `/api/v1/chats/{id}/cancel` | Cancel an in-flight request |
| `POST` | `/api/v1/chats/{id}/messages/{msgID}/regenerate` | Generate another answer for a turn |
| `POST` | `/api/v1/chats/{id}/messages/{msgID}/edit` | Re-ask a question with new wording |
| `POST` | `/api/v1/chats/{id}/messages/{msgID}/select` | Switch which alternative of a turn is visible |
| `GET` | `/api/v1/community/channels` | List channels |
| `POST` | `/api/v1/community/channels` | Create a channel |
| `DELETE` | `/api/v1/community/channels/{id}` | Delete a channel |
| `GET` | `/api/v1/community/channels/{id}/messages` | Read a channel |
| `POST` | `/api/v1/community/channels/{id}/messages` | Post to a channel |
| `POST` | `/api/v1/community/channels/{id}/read` | Update local read and mute state |
| `PUT` | `/api/v1/community/messages/{id}` | Edit your own message |
| `DELETE` | `/api/v1/community/messages/{id}` | Delete your own message |
| `POST` | `/api/v1/community/messages/{id}/moderate` | Tombstone a message as the room creator |
| `POST` | `/api/v1/community/sync` | Sync community events with peers now |
| `GET` | `/api/v1/contributions/leaderboard` | 30-day social recognition leaderboard |
| `GET` | `/api/v1/contributions/settings` | Read the contributions opt-out |
| `POST` | `/api/v1/contributions/settings` | Set the contributions opt-out |
| `POST` | `/api/v1/contributions/sync` | Sync receipts with peers now |
| `GET` | `/api/v1/metrics` | Local metrics |
| `POST` | `/api/v1/backup/export` | Export an encrypted backup |

### Compatible client API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/models` | List available models (OpenAI-compatible) |
| `POST` | `/v1/chat/completions` | Chat completion (OpenAI-compatible) |

### Bootstrap API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/bootstrap/v1/join` | Request admission to a room |

### Peer API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/peer/v1/membership/sync` | Exchange roster updates |
| `GET` | `/peer/v1/catalog` | Signed advertisements for this host's models |
| `POST` | `/peer/v1/inference` | Request inference from this host |
| `POST` | `/peer/v1/inference/cancel` | Cancel one of your own in-flight requests |
| `POST` | `/peer/v1/community/sync` | Exchange community events using per-author cursors |
| `POST` | `/peer/v1/contributions/sync` | Exchange contribution receipts using per-pair cursors |
| `POST` | `/peer/v1/contributions/ack` | Return a counter-signed receipt to the host |

### Runner API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/runner/v1/health` | Engine status |
| `POST` | `/runner/v1/models/load` | Launch the engine and wait for it to become healthy |
| `POST` | `/runner/v1/models/unload` | Stop and reap the engine |
| `POST` | `/runner/v1/engine/restart` | Restart the engine with the same load arguments |
| `POST` | `/runner/v1/inference` | Run inference on the loaded model |
| `POST` | `/runner/v1/inference/cancel` | Cancel the active inference |

<!-- endpoint-inventory:end -->
