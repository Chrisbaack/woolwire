# Feature guide

## Rooms and installations

An **installation** is one Woolwire app and its state database. A **room** is a
private group of installations. One installation belongs to one room at a time.

After owner setup, choose a display name and either host a room or paste a room
invitation. The creator can rotate the reusable invitation, enable approval
mode, approve pending members, and remove members in **Room Admin**. Rotation
changes who can join with the old code; it does not remove existing members.
If approval is enabled, wait for the creator to approve, then retry joining.

Hosting a room and hosting a model are independent. The creator is the admission
authority, not the room's central inference server. Existing members can keep
working without the creator if they can reach one another. Membership removals
cannot reach a partitioned peer until communication resumes.

**Connect Device** pairs another browser with the same installation. That
browser gets owner access to its chats and settings. A friend should instead
run their own installation and use the room invitation. A setup secret, an
invitation, a local API token, and a runner token serve different purposes;
see [Configuration](CONFIGURATION.md#credentials).

## The Meadow

The dashboard lists advertised models, their hosts, context limits, queue
estimates, and availability. Search by model or host name and use **Ready only**
to narrow the list. Room counters show models and hosts advertising them, not
a count of every online room member.

Choose **Start a conversation** to select a model in My Chats. Availability is
based on signed, refreshed advertisements; a clear queue is not a promise of
instant output. Network changes or another request may affect a model between
selection and dispatch.

## My Chats

Create a conversation, select a model, and send text. Responses stream as they
arrive. The conversation view supports code blocks and collapsible `<think>`
sections from models that emit them. A cancel action asks the serving host to
stop the request; an external backend may continue work after forwarding stops.

Saved conversations belong to your installation. Choosing another host sends
the conversation context to that host for the next request. The room creator
does not gain access to these conversations through room administration.

**No-save mode** keeps transcript bodies in memory instead of the messages table.
Conversation metadata can still be stored. Do not rely on browser navigation,
reload, or a process restart to preserve an ephemeral transcript. No-save mode
cannot control logging by an external model server or its operator.

The context gauge estimates tokens from text length. Generation statistics can
also be estimates based on streamed chunks; they are useful diagnostics, not
exact tokenizer counts or comparable benchmark results.

## Sharing an external model

Use **Settings** to register a server you already run. Woolwire supports text
chat completions over the OpenAI-compatible HTTP interface. Supply a base URL,
optional API key, and the backend's model name. Discovery tries model-list
endpoints; inference uses the chat-completions interface.

1. Make the server reachable from the Woolwire **process or container**.
2. Enter its base URL, for example `http://127.0.0.1:8081` for a native server
   on the same machine.
3. Test/discover models and select the exact backend model identifier.
4. Set a context limit and maximum generated tokens appropriate for the backend.
5. Enable and publish the model, then check the Meadow.

The friendly name can differ from `backend_model`. Woolwire's opaque `model_id`
is what room clients select; it is not sent as the backend's model name.
Private network destinations require the endpoint's explicit opt-in. On a
container bridge, `127.0.0.1` points at the container itself; see
[host reachability](GETTING_STARTED.md#external-servers-and-container-networking).

Unpublishing stops the model being offered to room requests, including local
room chat requests. Deleting an external model registration does not delete
anything from the external server.

## Managed GGUF models

The managed setup adds a runner that launches `llama-server`. The app downloads
and discovers weights; the runner reads them and executes inference.

In Settings, either browse an existing models directory or resolve a public
Hugging Face repository to choose a GGUF file. A Hugging Face cache under `hub/`
is understood without reorganizing it. Discovery reads GGUF headers, skips
non-language-model weights, and groups split models by their first shard.
Nearby projector and draft/MTP companion files are associated with the model.

Downloads are written in the same Hugging Face cache layout they are read from:
the bytes become `hub/models--org--repo/blobs/<etag>`, the snapshot entry
`snapshots/<commit>/<file>` is a symlink to them, and `refs/<branch>` records
the commit. Anything else on the machine built on `huggingface_hub` — Unsloth
Studio, `llama-cli`, a training script — then finds the same weights without a
second copy. A download whose URL names no repository, such as a mirror or a
direct link, lands as a plain file in the models directory instead. Weights an
earlier version of Woolwire downloaded flat are filed into the layout once, in
the background, on the next start.

Downloads run as jobs with progress and cancellation. A supplied SHA-256 is
checked. A digest identifies file content; it does not establish that a model
is safe or that its license allows your intended use. Discovery does not hash
every file already on disk. Downloaded models can be removed through the UI;
models found in a shared directory are never deleted by Woolwire.

Load a model to offer it. The current runner holds one loaded model at a time;
loading another replaces it and withdraws the previous advertisement. Unload
withdraws the model. The app and runner must see the same relative model paths.
Read-only storage allows discovery and serving but disables downloads.

Omitting `gpu_layers` lets the runner choose offload using its hardware. Explicit
`gpu_layers: 0` requests CPU execution. Container RAM/CPU ceilings still apply;
weights fitting on disk does not mean they fit in memory. The app container's
hardware report can differ from the runner's GPU view.

Attaching a projector prepares the engine, but the current UI and public chat
API accept **text content only**. Image uploads and multimodal message arrays
are not implemented.

## Sharing fairly

Local chats, external API clients, and peer requests use the host's shared queue.
Settings exposes active-request and queue limits plus queue and execution
timeouts. The defaults are one active request, one queued per member, ten
queued overall, a five-minute queue timeout, and a ten-minute execution timeout.
See [Configuration](CONFIGURATION.md#in-app-host-limits).

Container resource ceilings are configured in Compose. The design documents
also discuss schedules and more advanced resource controls; these are not
current host-limit API fields.

## Community and contributions

**Community** has shared channels, signed messages, author edits/deletions,
creator moderation, and manual synchronization. These are room messages, not
private direct messages. Replication depends on reachable peers retaining the
events. Default retention is 30 days or 250 MiB. Deletion cannot erase copies
someone kept outside Woolwire.

**The helping herd** recognizes shared compute with a rolling 30-day score.
Completed, acknowledged peer requests can earn points, capped at 20 per
requester/host pair per UTC day. Self-service does not score. Points are capped
recognition, not an uncapped request counter, currency, or access entitlement.
Contribution receipts expose the participating member relationship but exclude
prompt and response content. Opt out in Settings to stop participating in
public receipt scoring.

## Integrations and operations

The local `/v1/models` and `/v1/chat/completions` endpoints let compatible clients
use room models. They require a separate bearer token and the request header
described in the [API guide](API.md). This is a text chat subset, not the entire
OpenAI API.

Owner APIs also expose local JSON metrics and encrypted database exports.
[Backup and restore](BACKUP_RESTORE.md) explains how to preserve your identity,
room, settings, and saved conversations.
