# Opt-in search replacement for Responses to Chat

This contract covers Responses HTTP replies on routes that translate
to Chat Completions. The router waits for the model to request a search, makes
at most one search, and resumes the same model with the original call and its
result. [Responses SSE continuation](go-search-stream-runtime-contract.md)

The source shape follows the [Responses web search guide](https://developers.openai.com/api/docs/guides/tools-web-search?api-mode=responses).
Chat continuation uses the assistant tool call followed by a tool message with
the same call ID, as described in the [function calling guide](https://developers.openai.com/api/docs/guides/function-calling).
The backend speaks the [SearXNG JSON search API](https://docs.searxng.org/dev/search_api.html).

## Binding and admission

```toml
[search.backends.internal]
endpoint = "https://search.example.invalid/searxng"
count = 5

[search.bindings."public-search-model"]
tool = "web_search_preview"
backend = "internal"
```

- A binding names an enabled configured canonical model whose effective
  dialect is Chat and whose provider allows lossy translation. Aliases resolve
  through that canonical model. Discovery and dynamic models have no binding.
  Unknown keys, backend references, tools, and ineligible model keys fail
  config validation without I/O. The optional backend `headers` table is
  independent of model and client credentials and uses the search client's
  validated header boundary. Count is an integer in 1–10, default 5.
- Admission recognizes one exact tool definition, `{"type":"web_search_preview"}`,
  with absent or `"auto"` choice. This service substitution always produces an
  approximated loss at `tools[0]`. Strict routes and unbound routes refuse
  before I/O. The request cannot select a backend by name.
- The first slice admits text-only stateless history without tool calls,
  including ordinary text controls already supported by the bridge. It refuses
  additional tools, filters, location and context-size controls, forced choice,
  private state, and previous search items. An `include` list may
  contain only `web_search_call.action.sources`.
- Same-protocol traffic uses the existing native forwarding path. Chat
  `web_search_options` and other source tool types do not enter this plan.

The binder passes a typed binding into a pure decoder. Source JSON is parsed
only at the request boundary. The semantic plan contains the selected backend,
bounded count and reserved synthetic function identity. The generation request
carries one function taking exactly one required string member, `query`, with
no additional properties. Because this slice admits no other tools or history
calls, the reserved identity cannot collide with a client definition.

## Execution and return

Keep orchestration in the Responses-to-Chat handler beside the existing pin,
policy and transport. A standalone protocol codec cannot own the outer deadline
or generation slot. Keep the adapter confined to search HTTP and wire validation.

1. Pin the model and provider once, prepare the plan, and enforce all losses
   before upstream I/O. Send the existing Chat request with the synthetic
   function and auto choice. Ordinary model text can return without a search.
2. Decode the first Chat response into the existing response IR. A search
   requires exactly one function call with the planned name, a valid opaque
   call ID, and a closed JSON object containing one nonblank UTF-8 query of at
   most 500 runes. Refuse duplicate keys, unexpected calls, malformed wrappers,
   mixed answer/tool output and incompatible finish reasons before search I/O.
3. Execute the bound backend once under the same request context. Preserve the
   original call ID. Filter results at the adapter, and serialize accepted
   results as bounded structured tool data. Add a trusted function description
   telling the model it may cite result labels such as `[S1]` in its answer.
   Keep query and snippets out of logs. A backend failure ends with a sanitized
   error and makes no continuation request.
4. Send one continuation to the original endpoint with the original assistant
   call followed by its tool result, using the same provider credential and
   session. Disable further tool calls. Retain the result bytes for transport
   retries, which cannot rerun the search. The outer context and slot remain
   held. If a request sets an output token budget, subtract authoritative
   first-round output usage before sending the continuation; fail on exhausted
   or unrepresentable accounting. Reject further calls or malformed final
   responses without inventing a successful completion.
5. Aggregate input/output/total and supported detail counters from each actual
   model round with checked arithmetic. Search itself contributes no model
   tokens. Preserve the public model name, loss header, and source tool echo.
   If search ran, prepend one completed `web_search_call` with a router-issued
   item ID, action `search`, the actual query, and actual result URLs only
   when the source include requested them. Keep this typed in the Responses
   output stage. Inline annotations may be emitted only for visible markers
   that identify an actual result transferred to the model. Use character
   indices in the actual emitted text and URL/title from the result record.
   Unknown markers and model-written URLs never create annotations.

A normal no-call first response accounts for one model round and must remain
within the requested output budget. A search response
accounts for two model rounds and at most one backend call. With streaming, the
first model round still requests a complete response. A search triggers an SSE
continuation with terminal usage requested from the model.

## Offline acceptance

Use fake Chat and search servers. Capture both model requests and prove the
completed assistant call and tool result share the same opaque ID, while auth,
provider, endpoint and session stay fixed. Exercise no-call, strict/unbound,
unsupported controls, malformed and duplicate arguments, second-call refusal,
search failure, deadline/cancel, continuation retry without a second search,
usage overflow and budget exhaustion, source inclusion and exact Unicode
citation spans. Check that forged markers and URLs never acquire provenance.
Verify ordinary Responses and native hosted search still take their original
paths. Live service enablement is a separate acceptance step.
