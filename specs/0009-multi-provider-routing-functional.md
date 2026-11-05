# 0009 — Multi-provider routing (functional)

**Date:** 2026-11-03
**Technical spec:** specs/0009-multi-provider-routing-technical.md

## Context

The Gateway forwards every call to one upstream provider, set once in
config. An operator who wants some agents on Anthropic and others on
Mistral or Gemini has to run one Gateway per provider. An agent such as
opencode, set up with a Mistral model and pointed at a Gateway configured
for Anthropic, gets errors.

This spec lets one Gateway serve several providers at once, choosing the
provider for each call from the request itself: first from the model it
asks for, then from the shape of its URL.

### Change to a standing invariant

`AGENT.md`, `README.md`, `specs/README.md`, and the `security-review` and
`spec-review` skills say the caller cannot choose or influence the
upstream. This spec amends that. A caller now influences which provider
serves a call through the model it asks for and the API it calls, but
only among providers the operator has listed. A caller still cannot
supply a host, URL, or provider name, and cannot reach a provider the
operator did not configure. Those documents are updated in the same
change that implements this spec.

## Goals

- One Gateway serves several providers, choosing one per call.
- A caller that asks for a Mistral model reaches Mistral, a caller that
  asks for a Claude model reaches Anthropic, and a caller that asks for a
  Gemini model reaches Google's Gemini API, all through the same address.
- A request whose model is unknown or absent still reaches the right
  provider when its URL shape makes the API clear and the operator has
  named a default for that API.
- A request is never sent to a provider that cannot understand it.
- An existing single-provider configuration keeps working unchanged.
- Every call to every provider goes through every enabled stage, which
  reads it in its own format. No provider, format or operation is ever
  forwarded unread, and no setting exempts one.

## Non-goals

- **Amazon Bedrock.** Its request signing and binary streaming format need
  their own design: specs/0010.
- **Google Vertex AI.** Its short-lived credentials, operator-pinned
  project and region, and multiple model publishers need their own
  design: specs/0011. This spec only makes sure a provider can speak more
  than one API format later, so Vertex fits without rework.
- **Translating between API formats.** A call is forwarded in the format
  the caller sent.
- **Fallback, retry, or load balancing** across providers.
- **Choosing a provider by name, by header, or by agent type.**
- **Generic passthrough providers**, in a list or as the single provider.
  Every provider is one of the supported kinds, because the Gateway must
  be able to read every call it forwards.
- **Operations beyond those under *Supported operations*.** Batches,
  files, cached contents, fine-tuning, agents, OCR, moderation and the
  like are rejected. Supporting one later means adding it to every stage
  at once.

## Users and scenarios

**The opencode user on mixed models.** An operator lists Anthropic, Mistral
and Gemini in one Gateway. Each opencode project is set up with a
different model, and all of them point at the same Gateway address. Each
call reaches the provider that serves its model, with that provider's
key, and the user sees the responses they would get calling the provider
directly.

**The model listing.** opencode asks the Gateway which models are
available, in the OpenAI style. That request names no model, so the
Gateway sends it to the operator's default for the OpenAI-style API,
Mistral.

**The existing operator.** An operator with a single provider setting
upgrades and changes nothing. Every supported call in that provider's
format still goes to it, whatever its model.

**The developer pasting secrets into Mistral.** A developer on
`mistral-large-latest` pastes a customer's email and an AWS key into the
chat. Mistral receives placeholders, exactly as Claude would, and a
policy rule the operator wrote against Claude conversations applies to
this one unchanged.

## API formats

The Gateway recognizes three API formats from the request's URL and
headers:

| Format | Spoken by | Recognized from |
| --- | --- | --- |
| Anthropic | Anthropic | The Messages and token-counting URLs, or any request carrying Anthropic's version header |
| OpenAI-style | Mistral | The chat, fill-in-the-middle and embeddings URLs, and the model listing or a single model's details |
| Gemini | Google Gemini API | Any URL under Google's beta API, or any URL ending in a model name followed by a Gemini action (generate, stream, count tokens, embed one or a batch) |

A request carrying Anthropic's version header is always Anthropic format.
Otherwise the Gemini rule is checked before the Anthropic and OpenAI-style
URLs.

A request matching none of these has an unknown format. Where a format
keeps the model name:

- Anthropic and OpenAI-style: the `model` field of the JSON body.
- Gemini: the URL, in the segment after `models/`.

## Supported operations

Within each format, only these operations are forwarded. Each has a
format-neutral name that policy rules (specs/0004) and logs use:

| Format | Operation | Neutral name | Carries content |
| --- | --- | --- | --- |
| Anthropic | Messages | `messages` | yes |
| Anthropic | Count tokens | `count_tokens` | yes |
| Anthropic | List models, get one model | `models` | no |
| OpenAI-style | Chat completions | `messages` | yes |
| OpenAI-style | Fill-in-the-middle completions | `completion` | yes |
| OpenAI-style | Embeddings | `embeddings` | yes |
| OpenAI-style | List models, get one model | `models` | no |
| Gemini | Generate content, streamed or not | `messages` | yes |
| Gemini | Count tokens | `count_tokens` | yes |
| Gemini | Embed content, one or a batch | `embeddings` | yes |
| Gemini | List models, get one model | `models` | no |

What each stage reads in each of them is that stage's spec: filtering
(specs/0006), policies (specs/0004), tracing (specs/0003).

## Behavior

### Configuration

1. The operator lists providers. Each has a unique name, a kind
   (Anthropic, Mistral, or Gemini), a base URL, the name of the
   environment variable holding its API key, and the model names it
   serves. A model name is matched exactly and case-sensitively, or as a
   prefix when it ends in `*`.
2. A provider's kind decides the API format it speaks: Anthropic speaks
   the Anthropic format, Mistral the OpenAI-style format, Gemini the
   Gemini format. A provider speaks exactly one format in this version;
   specs/0011 introduces a provider that speaks several.
3. For each API format, the operator may name one listed provider as that
   format's default. The default must speak that format. A format with no
   default has no fallback.
4. A configuration with only the single provider setting names one
   provider, which must be of a supported kind (Anthropic, Mistral or
   Gemini); any other is a startup error naming it. That provider serves
   every call in its format whatever model it names: rules 6 to 9,
   including the repeated-model rejection, do not apply. Rules 10 and 13
   do: a request of another format, or an operation that is not
   supported, is rejected, and every caller-supplied credential is
   removed. Setting both the single provider and a provider list, in the
   file or through environment variables, is a startup error.

### Choosing a provider

5. The Gateway reads the request's API format from its URL and headers,
   then looks for the model name where that format keeps it. A request
   whose body cannot be read as JSON, or has no `model` field, or whose
   `model` is not text, has no model. A Gemini URL with no model segment
   has no model. A body that names the model more than once is rejected,
   so that the Gateway and the provider can never read different models
   from the same request.
6. **Model first.** If the request has a model, the Gateway takes, in list
   order, the providers whose model names match it. The first of those
   that speaks the request's format serves the call.
7. **Format guard.** If one or more providers match the model but none of
   them speaks the request's format, the call is rejected. It is never
   sent to a provider that would not understand it, and it does not fall
   back to the format's default. Example: an Anthropic-format request
   asking for `mistral-large-latest`.
8. **URL fallback.** If the request has no model, or its model matches no
   provider, the call goes to the default for its format.
9. If the format has no default in that case, the call is rejected.
10. A request of unknown format, or of a known format but an operation not
    under *Supported operations*, is rejected, whatever model it names:
    the Gateway either cannot tell whether any provider would understand
    it, or cannot read it in every stage. This is checked before the
    model, so such a request never reaches a provider or a later stage.
11. Requests are routed the same way whatever their method. A model
    listing, which names no model, goes to its format's default.

### Forwarding

12. The call is forwarded to the chosen provider's configured base URL,
    keeping the caller's path and query. The host always comes from
    operator config, never from the request.
13. Whichever provider serves a call, every caller-supplied credential is
    removed before forwarding: the `Authorization`, `Proxy-Authorization`,
    `x-api-key` and `x-goog-api-key` headers and a `key` query parameter.
    The provider's own key is then added in the form that provider
    expects.
14. **Every call goes through every stage.** Every enabled stage reads
    every forwarded call in its own format, whatever the provider:
    filtering redacts Mistral and Gemini requests as it does Anthropic
    ones, policy rules see their conversation in the same shape, and
    tracing records the same details. There is no setting that exempts a
    provider, a format or an operation from a stage that is on. Turning a
    stage off turns it off for every call.

### What other features see

15. A call refused by authorization is refused before routing; routing
    never sees it.
16. Policies, filtering and tracing see the provider the call was routed
    to. A policy rule that refers to the provider sees its name.
17. Each trace record names the provider that served the call. In list
    mode, the request record names the model the Gateway found, from the
    body or the URL, including for a call routing then rejected; in single
    mode it names the model as today. A call rejected before or by routing
    gets a request record with no provider field. In single mode this is
    one visible change from today: a call refused by authorization or
    identification no longer names the provider in its record.
18. Trace records read conversation and response details (tool results
    sent back, response identifier, model, stop reason, tokens, tool
    calls) in every format, streamed or not, as specs/0003 describes. A
    detail the provider's response does not carry is null.
19. Changes to the provider list take effect on restart. A policy reload
    does not change routing.

## Operator controls

| Control | What it does | Default | At the extremes |
| --- | --- | --- | --- |
| Provider list | The providers the Gateway can route to: name, kind, base URL, key variable, model names | Empty: the single provider setting applies | One entry: every call of its format whose model matches goes there. |
| Model names | Which models a provider serves; `*` at the end matches a prefix | None; at least one is required per provider | `*` alone matches every model, making the provider the first choice for its format. |
| Default per format | Where a call goes when its model is absent or matches no provider | None for any format | No defaults: every call must name a model a listed provider serves. |

With an operator who configures nothing, nothing changes: the single
Anthropic provider serves every call. Multi-provider routing is off until
the operator writes a provider list, which is the safe default because no
call can reach a provider the operator did not choose.

## Failure behavior

Routing fails closed. A call for which no provider can be chosen is
rejected before any provider is contacted. It is never forwarded to a
guess.

| Situation | Caller receives |
| --- | --- |
| Model matches a provider, but none of those speaks the request's format | 400: the model is not available for this API |
| Body names the model more than once | 400: the request names its model more than once |
| No model, or no provider matches it, and the format has no default | 404: no provider serves this request |
| Unknown format, or an operation not supported | 404: no provider serves this request |
| Supported content operation whose body a stage cannot read (not JSON, wrong shape, a repeated key) | That stage's failure: "governance unavailable" naming it; the provider receives nothing |
| Body over the Gateway's size limit | 413, as today: the call is refused before routing |
| Chosen provider unreachable | 502, as today; no other provider is tried |

Rejections use the Gateway's existing denial response: a JSON error
naming the routing step, a generic message, and the request identifier,
with the existing do-not-retry header. The response never names a
provider or base URL.

Misconfiguration fails at startup, never on a call: a duplicate or empty
provider name, an unknown kind (in a list or as the single provider), a
base URL that is not absolute or not
`https` (plain `http` is allowed only for localhost), a missing or unset
key variable, a provider with no model
names, a model name that is empty or has `*` anywhere but the end, a
default that names an unlisted provider or one of the wrong format, and
both the single provider and a list. Errors name the setting at
fault and never print a key.

## What the operator sees

- At startup, in list mode: one line per listed provider with its name,
  kind, model names and whether it is the default for its format. Never
  its key.
- For each rejected call: the Gateway's existing per-denial warning, which
  names the routing step, the request identifier and status, and a reason
  giving the rejection kind (format mismatch, repeated model, no provider,
  unknown format, or unsupported operation), the format, the HTTP method
  and path for an unsupported operation, and the model if one was found. Model
  names longer than 256 bytes are shortened to 256 bytes in logs and
  trace records, since the caller chooses them. Routing always matches
  the full name.
- For each routed call: the provider name in its trace records.
- A routing rejection is told apart from an upstream failure by its
  status and by naming the routing step. An upstream failure names a
  provider in its trace records.
- Nothing logged, traced or returned contains an API key, an
  `Authorization`, `x-api-key` or `x-goog-api-key` value, a `key` query
  parameter value, or prompt content. Model names are logged; they are not
  secret.

## Acceptance criteria

Unless stated otherwise, the Gateway lists Anthropic (`claude-*`), Mistral
(`mistral-*`, `codestral-*`) and Gemini (`gemini-*`). Mistral is the
OpenAI-style default, no other default is set, and filtering is on with
every category enabled.

1. **Given** that setup, **when** an OpenAI-style chat request asks for
   `mistral-large-latest`, **then** it is forwarded to Mistral with
   Mistral's key, and no other provider receives anything.
2. **Given** that setup, **when** an Anthropic Messages request asks for
   `claude-sonnet-5`, **then** it is forwarded to Anthropic with
   Anthropic's key.
3. **Given** that setup, **when** a request is sent to
   `/v1beta/models/gemini-2.5-pro:generateContent` with no `model` field
   in its body, **then** it is forwarded to Gemini with Gemini's key, and
   the request trace record names `gemini-2.5-pro` as the model.
4. **Given** that setup plus Anthropic as the Anthropic-format default,
   **when** an Anthropic Messages request asks for `mistral-large-latest`,
   **then** the caller receives 400 and no provider is contacted, not even
   the Anthropic default.
5. **Given** that setup, **when** an OpenAI-style chat request asks for
   `unknown-model`, **then** it is forwarded to Mistral, the OpenAI-style
   default.
6. **Given** that setup, **when** an Anthropic Messages request asks for
   `unknown-model`, **then** the caller receives 404 and no provider is
   contacted.
7. **Given** that setup, **when** a `GET /v1/models` arrives, **then** it
   is forwarded to Mistral.
8. **Given** that setup, **when** an OpenAI-style chat request has a body
   that is not JSON, **then** it is routed to Mistral (it names no
   model), the caller receives "governance unavailable" naming filtering,
   and Mistral receives nothing.
9. **Given** that setup, **when** a request arrives at a URL matching no
   known format, **then** the caller receives 404 and no provider is
   contacted.
10. **Given** two Mistral providers that both list `mistral-*`, **when** a
    request asks for `mistral-large-latest`, **then** the one listed first
    serves it.
11. **Given** a provider that lists `mistral-*` placed before one that
    lists `mistral-large-latest`, **when** a request asks for
    `mistral-large-latest`, **then** the first one serves it: list order
    wins, not how specific the match is.
12. **Given** only the single Anthropic provider setting, **when**
    Messages calls arrive for any model, **then** every one goes to that
    provider; **when** an OpenAI-style chat request or an Anthropic
    message-batches request arrives, **then** the caller receives 404 and
    the provider receives nothing.
13. **Given** both the single provider setting and a provider list, **when**
    the Gateway starts, **then** it exits with an error naming the
    conflict.
14. **Given** a caller that sends `Authorization`, `x-api-key` and
    `x-goog-api-key` headers and a `key` query parameter, **when** the call
    is forwarded to each kind of provider, **then** the provider receives
    only the Gateway's key for it, in that provider's form, and none of
    the caller's values.
15. **Given** that setup, **when** an OpenAI-style chat request to Mistral
    and a Gemini generate request each contain an email address, **then**
    each provider receives it replaced by an `[REDACTED:email]` placeholder,
    and the Gateway logged no warning at startup about either provider.
16. **Given** that setup and a policy denying any conversation that
    contains "Project Falcon", **when** that text is sent in an Anthropic
    Messages request, an OpenAI-style chat request and a Gemini generate
    request, **then** all three are denied with the same error and no
    provider is contacted.
17. **Given** that setup, **when** a call is routed to Mistral, **then**
    both of its trace records name `mistral` as the provider, and a policy
    rule that refers to the provider sees `mistral`.
18. **Given** that setup, **when** an Anthropic Messages request for
    `mistral-large-latest` is rejected by routing, **then** its request
    trace record shows a denial by the routing step, names
    `mistral-large-latest` as the model, has no provider field, and no
    response trace record is written.
19. **Given** a default that names a provider of a different format, **when**
    the Gateway starts, **then** it exits with an error naming the default.
20. **Given** authorization on and a caller with no valid credential,
    **when** it sends a request for a listed model, **then** it is refused
    by authorization and no routing warning is logged.
21. **Given** keys set for every provider and a caller sending its own
    credentials, **when** calls succeed, are rejected by routing, or fail
    upstream, **then** no key or credential value appears in any log line,
    trace record or response.
22. **Given** an OpenAI-style chat request whose body names `model` twice,
    **when** it arrives, **then** the caller receives 400 and no provider
    is contacted.
23. **Given** only the single provider setting with Mistral and filtering
    on, **when** a chat request contains an API key, **then** Mistral
    receives an `[REDACTED:api_key]` placeholder in its place.
24. **Given** only the single Anthropic provider and a body that names
    `model` twice, **when** it is sent to `/v1/messages`, **then** it is
    handled as before this spec, with no routing rejection.
25. **Given** a single provider set by environment variable and a provider
    list in the file, **when** the Gateway starts, **then** it exits with
    an error naming the conflict.
26. **Given** a request to
    `/v1beta/models/gemini-%2570ro:generateContent`, **when** it is routed,
    **then** the model the Gateway matches and records is `gemini-%70ro`,
    the same name Gemini reads from the URL.
27. **Given** the provider list is changed on disk and a policy reload is
    triggered, **when** the next call arrives, **then** it is routed by the
    list the Gateway started with.

28. **Given** only the single provider setting and authorization on, **when**
    a caller with no valid credential is refused, **then** its request
    trace record has no provider field, which it would have had before
    this spec.
29. **Given** that setup, **when** an Anthropic message-batches request, a
    Gemini files or cached-contents request, or a Mistral agents request
    arrives, **then** the caller receives 404, no provider is contacted,
    and the routing warning gives the reason "unsupported operation" with
    the method and path.
30. **Given** a single provider whose name is not `anthropic`, `mistral`
    or `gemini`, **when** the Gateway starts, **then** it exits with an
    error naming it.
31. **Given** that setup and tracing on, **when** a streamed Mistral chat
    call returns a tool call and token usage, **then** its response trace
    record carries the tool call and the sent and received token counts.

## Open questions

- **Merged model listing.** Should a model listing combine the models of
  every listed provider instead of going to one default? Deferred to a
  later spec. Product decides.
