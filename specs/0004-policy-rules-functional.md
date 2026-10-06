# 0004 — Policy rules (functional)

**Date:** 2026-10-06
**Technical spec:** specs/0004-policy-rules-technical.md

## Context

The staging pipeline (specs/0001) has a slot for a policies stage, and
identification (specs/0002) and tracing (specs/0003) now tell the Gateway
who is calling and what the call contains. But the policies stage has
nothing to enforce: an operator cannot say "this agent may use only these
models", "this agent may never be offered the `delete_database` tool", or
"no agent may send a file path under `/secrets` to the LLM". Every call
that reaches the Gateway is forwarded, whatever it asks for.

Hard-coded checks would not scale: every operator's rules differ, and they
change often. Operators need a real policy language, written by them,
versioned in their own repository, and changeable without restarting the
Gateway and dropping in-flight calls.

**Where the line is drawn.** The Gateway controls what **goes out**: every
request from the agent to the LLM provider, which carries the prompt, the
files the agent read, the results of the tools it ran, and the list of
tools it offers. The agent, with whatever sandbox the operator runs it in,
controls what it **executes**: the Gateway is not in the execution path, so
it cannot prevent a tool from running, and a sandbox is the right place to
stop `rm -rf`. Policies therefore decide on requests only. They can still
see the tool calls and results already in a conversation, so they can
detect that an agent did something it should not have, and refuse to carry
that conversation any further (see *Users and scenarios*). Responses stay
exactly as specs/0001 and specs/0003 describe: always relayed unchanged,
and always traced.

## Goals

- Let the operator decide, by writing rules, whether a request is allowed,
  using anything that is known about it: who the agent is, which model and
  operation it asks for, which tools it offers and has used, with which
  arguments, and the content of the conversation.
- Write those rules in **OPA's policy language (Rego)**, with no
  Gateway-specific dialect, so operators can use OPA's own tooling and
  knowledge to write and test them.
- Give the rules to the Gateway through **configuration**: the config names
  the policy file.
- Let the operator **reload** the policy from disk without restarting, on
  demand or automatically, and never end up with a half-applied or broken
  policy.
- Deny exactly as the pipeline already denies (specs/0001): a generic
  caller-visible error, an audit-only reason, a per-call identifier.
- Behave exactly as today until the operator configures a policy.

## Non-goals

- Deciding on **responses**. Holding, blocking or evaluating a response —
  including the tool calls a model requests in it — is not part of this
  version, and there are no response-side rules. The Gateway is not where
  tool execution is secured (see *Context*), and holding tool calls back
  would bring a buffer, mid-stream errors and retry loops into a path that
  today streams untouched. Responses continue to be fully recorded by
  tracing (specs/0003), unchanged.
- Preventing a tool from running. The Gateway sees a tool call in the
  request that carries its result, after the agent has run it. Rules can
  refuse to continue the conversation, not undo what ran.
- Modifying a request (redaction, compaction, rewriting arguments). Rules
  only allow or deny; rewriting is the filtering spec's.
- Per-agent rate limits and quotas, and counting across calls. Policies see
  one call at a time and keep nothing between calls, like tracing
  (specs/0003, rule 1). Rate limiting and circuit breaking are their own
  features.
- Fetching policies from a remote service (OPA bundles, an OPA server, a
  git repository). In this version policy is files on local disk.
- Reloading the rest of the configuration. Only policy files reload;
  changing any other setting still needs a restart.
- A rule testing tool, policy editor or management API. Operators use
  OPA's own tools to test their Rego.
- A "dry run" or "would have denied" mode that logs without enforcing.
  Worth having, but its own decision; see *Open questions*.
- Free-text deny messages chosen by the policy author, for the caller or
  the logs; see *What the operator sees* for why.
- Rules that depend on caller-claimed identifiers for security. A session
  (specs/0003) is available to rules but is only a claim by the agent.

## Users and scenarios

**The operator writing a policy.** `agent-alpha` is a coding assistant
and `agent-beta` is a support bot. The operator writes one Rego file that
says: `agent-beta` may use only the small model; no request may offer a
tool named `delete_database`; no request may carry text matching a
credential pattern; and calls made on weekends by any agent other than
`agent-alpha` are denied. They point the Gateway's configuration at the
file and start it. From then on every request that breaks a rule is denied
at the Gateway, and the logs say which rule did it.

**The operator tightening a policy during an incident.** An agent is
misbehaving at 2 a.m. The operator edits the policy file to deny that
agent entirely and sends the reload signal. Within a moment the new policy
applies to new calls; calls already in flight finish under the policy they
started with. A typo in the edit does not take effect: the previous policy
keeps running and the log says the reload failed and why.

**The operator who wants an agent stopped after a violation.** The agent
runs in a sandbox that should have blocked `rm -rf`, but a prompt-injected
model got the agent to try it, and the attempt is now in the conversation.
The operator writes a rule: deny any request whose conversation contains a
`bash` call with a destructive command. The agent's next request carries
that history, so it is denied, and so is every later one in that
conversation. The Gateway has not prevented the attempt (the sandbox's job)
but it has stopped the agent from continuing, and the trace shows exactly
where it happened. The history travels with every request, so no memory is
needed in the Gateway.

**The operator who limits what an agent may be given.** They write a rule
that denies any request offering a tool named `delete_database`. The agent
developer has to configure the agent without it: the capability is
refused at the point where the agent hands it to the model.

**The agent developer.** A permitted call behaves exactly as without
policies. A denied call returns the Gateway's standard denial (specs/0001):
the stage that denied, a generic message, and a per-call identifier to
quote to their operator. They cannot tell which rule matched, so they
cannot map the policy by probing.

## Behavior

### Where policies sit

1. Policies run after identification and tracing in the fixed pipeline
   order (specs/0001, rule 2; specs/0003). Everything identification
   established and the session tracing read are available to the rules.
2. Policies add no new way to deny: a denial uses the contract of
   specs/0001 (rules 5–8) unchanged — a caller-visible status and generic
   message naming the policies stage, a per-call identifier generated by
   the Gateway, and an audit-only reason. The status of a policy denial is
   always **403 (Forbidden)**, whichever rule fired, so the caller cannot
   tell rules apart by status, and so standard clients treat it as a
   refusal and not as a transient error worth retrying. Like every
   deliberate denial, it carries specs/0001's "do not retry" signal.
3. A request denied by a policy is never forwarded to the provider, and no
   later stage examines it (specs/0001, rule 6). Every enabled stage still
   observes the outcome (specs/0001, rule 10).

### Writing rules

4. Policies are written in Rego, evaluated with OPA's semantics. Every
   OPA built-in function that works without external access is available
   (string, regex, number, set, time, JSON and so on). Built-ins that
   reach out of the process (HTTP requests, environment variables, the
   filesystem) are unavailable: a policy that uses one is invalid.
5. The operator names **one policy file** in configuration, a `.rego`
   file holding all the rules.
6. The operator may also name a **data** location: a JSON or YAML file, or
   a directory of them, made available to rules as OPA's `data`. This is
   where allow lists, per-agent settings and the like live, so they can be
   edited without touching the rules. Each file's top-level keys appear
   directly under `data` (a file `{"admins": [...]}` is `data.admins`); two
   files may not define the same top-level key, and the key `cardea` is
   reserved for the policy itself.
7. Rules are written in the Rego package `cardea.policy` and answer
   through one set of deny entries, `deny`. Each entry is a short string,
   the **rule identifier** of the rule that fired, chosen by the operator
   (for example `"no-delete-tool"`): letters, digits and `. _ : -`, at most
   64 characters, starting with a letter or digit. An entry that does not
   fit is recorded as `invalid-rule-id` and still denies. A request is
   **denied if the set is not empty** and **allowed if it is empty**. A
   policy may leave the set undefined.
8. Denying by default is the operator's choice, not the Gateway's: an
   operator who wants "deny unless explicitly allowed" writes an `allow`
   rule of their own and one `deny` entry for `not allow`. This keeps
   rule 9 consistent with specs/0001, rule 4.
9. With no policy configured, the stage allows every call and adds no
   latency worth measuring. A configured policy with no `deny` entry
   firing allows the call.
10. Every fired entry is recorded, not only the first: if three rules
    match, the audit trail lists all three identifiers. The caller still
    sees one generic denial.

### What rules can see

11. For every evaluation, the rules receive one input document describing
    the request:

    | Key in `input` | What it holds |
    | --- | --- |
    | `call` | `id`, the per-call identifier; `size_bytes`, the request size; `headers`, the caller's headers (lower-case names, each a list of values), without the credential headers `Authorization`, `Proxy-Authorization`, `X-Api-Key`, `Api-Key`, `Cookie` and `Set-Cookie`. |
    | `agent` | `id`, the identified agent, if identification is enabled (specs/0002). |
    | `session` | `id`, the session identifier, if the operator configured where to find one for that agent (specs/0003): a claim by the agent, not a verified fact. |
    | `provider` | The provider name. |
    | `operation` | `name` (`messages`, `count_tokens` or `other`), plus the raw HTTP `method` and `path`. |
    | `model` | The model name the caller asked for. |
    | `stream` | Whether the call asks for a streamed response. |
    | `parameters` | The other generation settings as sent (`max_tokens`, `temperature`, and the like). |
    | `system` | The system prompt, as text. |
    | `messages` | Each message in order: `role`, and `content`, a list of parts each with a `type` (`text`, `image`, `tool_call`, `tool_result`, or the provider's own name for anything else) and its text, tool-call or tool-result fields. |
    | `tools` | Every tool the agent makes available to the model: `name`, `description` and `parameters` (its declared schema). |
    | `tool_calls` | Every tool call in the conversation, in order: `id`, `name`, and `arguments` (**argument values**, as structured data). These calls have already been made by the agent. |
    | `tool_results` | Every tool result in the conversation: `tool_call_id`, `tool_name` when the matching call is in the same request, `is_error`, `size_bytes`, and `text`. |

    Nothing about the response to *this* request exists yet, so nothing of
    it is in the input; earlier responses are visible only as the
    assistant messages and tool calls the conversation carries back.
12. The input is the complete data the Gateway has about the call, in one
    shape that does not depend on the provider's wire format, so a rule
    does not change when a provider is added. A field that does not apply
    or is not known is absent, never an empty string standing in for
    "unknown": for example `agent` when nobody was identified, `session`
    when none was found, `system` when there is no system prompt, and every
    conversation key for an operation that carries no conversation. A rule
    that tests an absent field does not match. A list the Gateway knows is
    empty is present and empty (`tools`, `tool_calls` and `tool_results`
    as `[]`, `parameters` as `{}`), so `count(input.tool_calls) == 0` is
    true for a request with no tool calls.
13. The current time and date are available through OPA's own time
    functions, so rules may depend on the hour, day of week or date.
14. **Rules see argument values and conversation text; nothing else does.**
    They exist only in memory for the duration of the evaluation. They are
    never written to a log, a trace record, a metric, a reload message or
    an error returned to the caller. This differs deliberately from
    tracing (specs/0003), which records argument names, sizes and
    fingerprints only.
15. When identification is disabled, the agent is absent from the input,
    and every rule on the agent simply does not match. A rule meant to
    deny unidentified calls must say so explicitly. At startup, the
    Gateway warns when a policy is configured and identification is
    disabled.

### Requests

16. Every request is evaluated against `deny` before any later stage and
    before the provider sees it, whether or not it asks for streaming.
17. The tool calls and tool results in the conversation have already
    happened: rules on them detect and stop the conversation, they do not
    prevent the call. Because the conversation is resent in full with each
    request, such a rule keeps denying every later request of that
    conversation without the Gateway remembering anything.

### Time limit

18. Each evaluation has a time limit. An evaluation that exceeds it is a
    **failure**, not a decision (see *Failure behavior*), and the caller is
    answered at the limit, not when the evaluation eventually ends: the
    Gateway cancels it, and one that cannot be interrupted at once is left
    to finish in the background without holding the request. If too many
    evaluations are still running in the background, new ones fail
    immediately instead of piling up. This answers the question
    specs/0001 left open for this stage.

### Loading and reloading

19. The policy is loaded and checked when the Gateway starts. If any file
    is missing, unreadable, does not parse or compile, uses an unavailable
    built-in, or defines no `deny` rule in the policy namespace (a policy
    that could never deny, usually because the namespace is misspelled),
    the Gateway **does not start** and the error names the file and
    location. Once loaded, the policy and data are
    held in memory: a request never reads the disk or recompiles the policy,
    and editing a file has **no effect** until a reload (rule 20) picks it
    up.
20. A policy can be reloaded from disk while the Gateway runs, two ways:
    - **On demand**: the operator sends the process the standard reload
      signal (SIGHUP).
    - **Automatically**: when the operator turns on watching, the Gateway
      reloads when a policy or data file is added, changed or removed.
      Changes arriving close together (an editor writing a file in
      steps, a deploy touching many files) cause one reload after they
      settle.
21. A reload reads all policy and data files afresh and checks them as at
    startup (rule 19). **Only if the whole set is valid** does the new
    policy replace the old; otherwise the old policy stays active, the
    reload is logged as failed with the reason, and nothing changes for
    callers. A partial policy is never applied.
22. The switch is atomic. A call evaluated before the switch uses the old
    policy, one after it uses the new, and a call is never evaluated
    against a mixture. An evaluation already running when a reload happens
    finishes under the policy it started with. Reloads never interrupt an
    in-flight call.
23. Every policy has a **revision**, a short identifier derived from the
    contents of all policy and data files. Reloading unchanged files keeps
    the revision and is a no-op. Every denial and every reload names the
    revision in force.
24. If a watched file disappears, or the policy location becomes
    unreadable, that is a failed reload (rule 21), not an empty policy:
    deleting the policy never silently turns enforcement off.
25. Reload does not re-read any configuration other than the policy and
    data contents. Changing the policy *locations*, the time limit or the
    watch setting needs a restart.

## Operator controls

| Control | What it does | Default | At the extremes |
| --- | --- | --- | --- |
| Policy file | The Rego file holding the rules | None (no policy) | Empty: policies allow everything. |
| Data location(s) | JSON or YAML file(s) or directory(ies) exposed to rules as data | None | Empty: rules see no extra data. |
| Reload on change | Watch policy and data files and reload when they change | Off | Off: reload only by signal or restart. On: every saved change applies automatically, valid or not-yet-valid (invalid ones are rejected per rule 21). |
| Reload settle time | How long to wait after the last change before reloading | 500 ms | Too low: reloads mid-write fail and are retried. Too high: slow to apply an urgent change. |
| Evaluation time limit | Longest one evaluation may take | 100 ms | Too low: heavy rules are denied as failures. Too high: a pathological rule holds every call open. |
| Stage on/off | Whether the policies stage is in the path (specs/0001) | On | Off: no rule is evaluated, whatever is configured. |

With an operator who configures nothing, the stage is in the path and
allows every call: it has been given nothing to enforce (specs/0001,
rule 4). Pointing the configuration at a policy is the deliberate act that
turns it into a gate, so an upgrade cannot start denying traffic by itself.

The body limit from specs/0001 also governs how much of a request rules can
inspect; a larger request is denied before any stage runs.

## Failure behavior

**Calls fail closed; reloads fail safe.**

- **Evaluation cannot finish** — time limit exceeded, too many abandoned
  evaluations still running, internal error,
  a decision that is not in the expected shape (for example `deny` is
  not a set of strings), or a request body the rules cannot read (an
  operation that carries a conversation, with a body that is not valid
  JSON or that repeats a key inside any object, since the rules and the
  provider could then read different values): the call is **denied**, not forwarded. The caller
  gets specs/0001's "governance unavailable" error naming the policies
  stage, with status **503 (Service Unavailable)**, distinguishable from a
  deliberate denial, which is always 403 (rule 2).
- **Rules that simply do not match** are not failures. Rego treats a rule
  that references an absent field as undefined, so it does not fire; a
  call matching no rule is allowed.
- **Policy invalid at startup**: the Gateway does not start (rule 19).
- **Policy invalid on reload**: the previous policy stays in force and a
  failure is logged (rule 21). The operator's feedback is the log, and the
  reload signal's effect is visible there. This is deliberate: a typo
  during an incident must not become an outage or, worse, disable
  enforcement.
- **No valid policy was ever loaded and one is configured**: cannot happen
  while running, because startup fails first.
- **The file watcher cannot start** (for example the operating system
  refuses to watch a directory): the Gateway keeps serving and enforcing
  the loaded policy, logs the error, and reload on the signal still works.
- **A reload signal with no policy configured** does nothing and is logged
  as such.

Responses are not affected by any of this: a policy failure never touches a
response, since policies never see one.

## What the operator sees

- **Denial**: one log line per denied call, per specs/0001, naming the
  per-call identifier, the stage, the agent (when identified), the policy
  revision, and the **rule identifiers** of every entry that fired. The
  caller sees none of this (rule 2).
- **Failure to decide**: one log line per failed evaluation, naming the
  revision and the kind of failure (time limit, internal error, malformed
  decision) — distinguishable from a denial.
- **Reload**: one log line per reload attempt: what triggered it (signal,
  file change), whether it succeeded, the old and new revision, and on
  failure the file and position of the first error. A reload that changed
  nothing is logged at a quieter level.
- **Startup**: one line naming the revision loaded and how many data files
  it came from, and the warning of rule 15 when it applies.
- **Allowed calls** add no log line (specs/0001). In the trace (specs/0003)
  the policies stage entry of the request record carries the revision in
  force and the rule identifiers that fired, in its `detail`.
- **Responses** keep being traced in full by specs/0003, exactly as
  before: policies add nothing to a response record and remove nothing
  from it.

Why rule identifiers only, and no policy-authored message: a Rego rule can
build a string from any input, including an argument value or a line of the
prompt. If the Gateway logged policy-authored text, a careless rule would
write prompt content into the logs, which this product must never do.
Rule identifiers are operator-chosen labels and are the only text from the
policy that the Gateway surfaces. The Gateway adds no prompt content,
argument or result values, API keys or `Authorization` headers to a log,
trace record or caller-visible message, and parse or compile errors quote
the policy file's own position and text, never any call's content. What
the Gateway cannot do is stop an operator from writing a rule whose
identifier is built from the request: such an identifier would carry that
text into the logs, and it is the operator's responsibility to write fixed
labels.

## Acceptance criteria

1. **Given** no policy configured (the default), **when** any request is
   proxied, **then** it is forwarded and behaves exactly as in
   specs/0001 criterion 1.
2. **Given** a policy whose `deny` has no entry that fires for a call,
   **when** that call arrives, **then** it is forwarded, the caller
   notices no difference, and no log line is added.
3. **Given** a `deny` rule keyed on the model name, **when** a request
   names that model, **then** it is denied, the provider receives nothing,
   and the caller gets a 403 with the "do not retry" signal and the generic
   denial naming the policies stage and a per-call identifier.
4. **Given** a rule keyed on the agent identifier (identification
   enabled), **when** the named agent calls, **then** it is denied, and
   **when** a different agent calls, **then** it is allowed.
5. **Given** a rule keyed on a tool's name and an argument value (for
   example `bash` with a command containing `rm -rf`) and a request whose
   conversation contains such a tool call, **when** it arrives, **then**
   it is denied; **given** the same tool with a harmless command,
   **then** it is allowed.
6. **Given** a rule that denies a request offering a tool of a given name
   in its list of tools, **when** a request offers it, **then** it is
   denied even though the conversation never uses it.
7. **Given** a rule keyed on the operation requested, **when** a call
   makes that operation, **then** it is denied, and calls to other
   operations are allowed.
8. **Given** a rule keyed on a request header, **when** a request carries
   it, **then** it is denied; **given** a rule that reads `Authorization`
   (or `X-Api-Key`, `Cookie`), **then** the header is absent from the
   input and the rule does not match.
9. **Given** a rule that uses an absent field (an agent when identification
   is disabled), **when** a call arrives, **then** the rule does not fire
   and the call is allowed.
10. **Given** a rule that depends on data from the configured data
    location, **when** the data file is changed and reloaded, **then** the
    next call is decided on the new data.
11. **Given** three rules that all match one call, **when** it is denied,
    **then** the log line lists all three identifiers and the caller sees
    one generic denial.
12. **Given** a rule denying any conversation that contains a forbidden
    tool call, **when** two successive requests of that conversation
    arrive, **then** both are denied, and the Gateway kept nothing
    between them.
13. **Given** a rule keyed on a credential pattern in the conversation
    text, **when** a request carrying it arrives, **then** it is denied,
    and the matched text appears in no log, trace record or response.
14. **Given** an evaluation that exceeds the time limit, **when** a call
    arrives, **then** it is denied with a 503 "governance unavailable"
    error without the "do not retry" signal rather than forwarded, and the
    log distinguishes this from a deliberate denial, which is a 403.
15. **Given** a policy file with a syntax error or a forbidden built-in,
    **when** the Gateway starts, **then** it exits with an error naming
    the file and position.
16. **Given** a running Gateway and a valid edit to the policy, **when**
    the operator sends the reload signal, **then** the next call is
    decided under the new policy and the log shows the old and new
    revision.
17. **Given** a running Gateway and an invalid edit, **when** the operator
    sends the reload signal, **then** the previous policy stays in force,
    calls are decided as before, and the log reports the failed reload
    with the file and position of the error.
18. **Given** watching enabled, **when** a valid change is saved to a
    policy file, **then** the policy reloads without a signal; **when** an
    invalid change is saved, **then** it is rejected as in criterion 17.
19. **Given** watching enabled, **when** several files change within the
    settle time, **then** exactly one reload occurs.
20. **Given** an evaluation in progress when a reload swaps the policy,
    **when** it finishes, **then** it was decided entirely under the
    policy it started with.
21. **Given** reload of files whose contents have not changed, **when**
    it runs, **then** the revision is unchanged and the reload is logged
    as a no-op.
22. **Given** a policy file is deleted while running, **when** a reload
    is triggered, **then** it fails as in criterion 17 and enforcement is
    not turned off.
23. **Given** a denial or failure of any kind and rules with fixed
    identifiers, **when** the logs, trace records and caller response are
    inspected, **then** no argument value, prompt text or credential
    appears in any of them, only rule identifiers.
24. **Given** the policies stage disabled in configuration, **when** a
    call that a rule would deny arrives, **then** it is forwarded.
25. **Given** any policy, **when** a response arrives — streamed or not,
    carrying a tool call or not — **then** it is relayed byte-identical to
    what the provider sent, and its `response` record is written exactly
    as specs/0003 defines.
26. **Given** a request to an operation that carries a conversation whose
    body is not valid JSON, or repeats a key in any object (for example
    `model` twice), **when** a policy is configured, **then** it is denied
    with the 503 "governance unavailable" error and not forwarded.
27. **Given** a request denied by a policy, **when** its `request` record
    is inspected, **then** the policies stage entry has `decision`
    `denied`, and its `detail` has the policy revision and the identifiers
    of the rules that fired.

28. **Given** a rule whose evaluation cannot be interrupted before the
    time limit, **when** a request arrives, **then** the caller receives
    the 503 at about the limit, not when the rule finishes, and the
    request is not forwarded.
29. **Given** the maximum number of abandoned evaluations still running,
    **when** another request arrives, **then** it is denied immediately
    with the 503 and the log says the Gateway is overloaded.

30. **Given** a policy file whose package name is misspelled so that it
    defines no `deny` in the policy namespace, **when** the Gateway starts
    or reloads, **then** it refuses to start (or keeps the previous policy
    and logs the failure), naming the file.

## Relationship to 0001, 0002 and 0003

- **0001.** This spec fills the policies slot. It changes none of 0001's
  rules: policies deny requests through the existing contract, and
  responses stay observe-only. It builds on the amendment 0001 now makes,
  that deliberate denials carry a "do not retry" signal and failures do
  not (rules 7 and 9, criterion 13). It also answers the question 0001 left open
  for this stage: how long an evaluation may take (rule 18).
- **0002.** Rules see the identified agent. 0002 left open what a stage
  that needs the agent does when identification is off; here: the agent is
  absent from the input and agent rules do not match (rule 15).
- **0003.** The session is available to rules as a claim. Tracing's own
  rule that arguments are never recorded still holds; rules read them in
  memory only (rule 14). Responses are traced exactly as before. The only
  change to the trace is the `detail` of the policies stage entry (the
  revision and the rule identifiers), which 0003 now provides for.

## Open questions

- **Dry-run mode.** Should an operator be able to run a new policy in a
  "log what it would deny, deny nothing" mode before enforcing it? It is
  the safe way to introduce a rule to live traffic, and would likely be a
  per-policy or per-rule setting.
- **A policy-authored message.** Should a rule be able to supply a reason
  string for the log? Excluded here because it could carry call content;
  one option is a fixed vocabulary of reason codes the operator
  declares in the data location.
