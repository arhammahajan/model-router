# Versioned and hosted tool boundaries

The offline suite checks eight named tools and one unknown version. Each named
tool has a native pass-through case and two translated refusal cases, one per
compatibility mode. The unknown version has the two refusal cases.

| Source contract | Execution owner | Native route | Translated route without an explicit mapping |
| --- | --- | --- | --- |
| Anthropic `bash_20250124` | Harness | Forward | Refuse |
| Anthropic `text_editor_20250728` | Harness | Forward | Refuse |
| Anthropic `computer_20250124` with the version beta header | Harness | Forward | Refuse |
| Anthropic `text_editor_20990101` (synthetic unknown version) | Unreviewed | Outside this matrix | Refuse |
| Responses `file_search` with vector store IDs | Provider | Forward | Refuse |
| Responses `web_search_preview` | Provider | Forward | Refuse |
| Gemini `googleSearch` | Provider | Forward | Refuse |
| Gemini `urlContext` | Provider | Forward | Refuse |
| Gemini `fileSearch` with store resource names | Provider | Forward | Refuse |

An enabled Responses search replacement is a separate route contract. It
substitutes one configured backend on a bound lossy Chat route.
[Search replacement contract](go-search-runtime-contract.md)

Native fixtures assert the tool declaration reaches a fake provider. They check
forwarding independently of provider and model availability. Translated
fixtures assert HTTP 400, a tool-specific diagnostic, and zero upstream
requests. The router leaves versioned client-tool execution with the harness.

Run the [HTTP conformance suite](../go/conformance/run.py) against the Go binary.
Fixture names beginning with `m5_tool_boundary_` cover this matrix.

Wire references. [Anthropic versioned computer tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/computer-use-tool), [OpenAI File Search](https://developers.openai.com/api/docs/guides/tools-file-search), [Gemini GenerateContent tool schema](https://ai.google.dev/api/generate-content)
