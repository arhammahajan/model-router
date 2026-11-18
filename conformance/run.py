#!/usr/bin/env python3
"""Portable external HTTP conformance runner (milestone 1).

Launches a fake upstream plus either backend (the Python router or the Go
binary), sends one client request per case, and asserts both the upstream
wire requests and the client-visible output.

Usage:
  python3 run.py --backend python|go [--go-bin PATH] [--python-bin PATH]
                 [--case NAME] [--list]

Cases live in cases/*.json. Each case specifies the router TOML (with
{UPSTREAM} for the fake upstream URL), the scripted upstream behavior, the
client request, and backend-agnostic expectations. Nondeterministic IDs and
timestamps are never asserted literally; identity *relationships* (call IDs,
item IDs, chunk IDs, sequence numbers) are asserted structurally instead.

Optional structured SSE expectations (expect.sse), all keys optional:

  {
    "kind": "responses" | "chat",
    "events": [...],       # exact ordered frame tokens; Chat data-only
                           # frames are "data", the terminal is "[DONE]"
    "absent": [...],       # tokens that must not appear
    "counts": {...},       # token -> exact count
    "done": true|false,    # require/forbid a terminal "data: [DONE]"
    "sequence_numbers": true,  # every JSON frame carries
                           # sequence_number == its 0-based position
                           # (Responses contract)
    "integrity": "responses" | "chat" | "anthropic" | "gemini"
                           # relationship checks: item/delta/done/completed
                           # identity consistency and terminal output that
                           # repeats (not duplicates) earlier deltas (responses);
                           # stable chunk IDs, stable tool-call indexes/IDs and
                           # a non-null terminal finish reason (chat);
                           # one message_start/stop pair, event/payload type
                           # agreement, block lifecycle with matching delta
                           # types, unique tool ids with exact JSON-object
                           # arg reassembly, and required terminal usage
                           # with stop_reason (anthropic);
                           # data-only SSE frames shaped as partial
                           # GenerateContentResponses: candidate index 0 /
                           # role model, stable modelVersion/responseId when
                           # present, text plus complete functionCall parts
                           # with object args and unique call IDs/names in
                           # order, exactly one terminal finishReason on the
                           # last frame, authoritative terminal usageMetadata
                           # with total == prompt+candidates+thoughts+tool-use,
                           # and no [DONE] sentinel (gemini)
  "joined_tool_args": {"0": "<args>"}
                           # chat/anthropic: fragments at each tool index must join
                           # to exactly this string (chunking-independent)
  "joined_args": {"0": {...} | "<json-object>"}
                           # gemini: complete functionCall args objects in order
                           # of appearance must equal these exactly; absent
                           # args default to {} (chunking-independent)
  "joined_text": "<text>"
                           # gemini: concatenated text deltas must equal this
  }

  Optional JSON-array expectations (expect.gemini_array), all keys optional:

  {
    "present": true,         # body must be one JSON array (closing bracket)
    "joined_args": {...},    # same object equality as the SSE validator
    "joined_text": "<text>"  # same text reassembly as the SSE validator
  }

  Optional upstream query expectations (expect.upstream), all keys optional:

  {
    "query": "alt=sse",      # exact raw query string forwarded upstream
    "query_absent": ["key"], # param names that must never be forwarded
                             # (client ?key= credentials are auth-only)
  }
  When "query"/"query_absent" are absent nothing is asserted (baseline
  compatibility for cases recorded before query capture).

Framing notes: only a line that *begins* with "event:"/"data:" opens a
frame, so the same tokens inside a JSON string are never misread as
framing. The runner captures a bounded client body per case; production
streaming code must stay incremental and must not buffer unbounded streams.
"""
from __future__ import annotations

import argparse
import http.server
import json
import os
import re
import socket
import subprocess
import sys
import threading
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
LEGACY_REPO = os.path.abspath(os.environ.get(
    "MODEL_ROUTER_LEGACY_REPO", os.path.join(os.path.dirname(REPO), "model-router-legacy"),
))
CASES = os.path.join(HERE, "cases")


def default_python() -> str:
    """Resolve the Python-router interpreter without hardcoding site paths.

    Precedence: $MODEL_ROUTER_PYTHON, the legacy checkout .venv, then the current
    interpreter. Callers (humans or parent agents) point at the original
    repo interpreter, e.g. MODEL_ROUTER_PYTHON=/path/to/dev/model-router-legacy/.venv/bin/python.
    """
    env = os.environ.get("MODEL_ROUTER_PYTHON")
    if env:
        return env
    import pathlib as _pl
    venv_py = os.path.join(LEGACY_REPO, ".venv", "bin", "python")
    if _pl.Path(venv_py).exists():
        return venv_py
    return sys.executable


def parse_sse(text: str, max_frames: int = 10000) -> list[dict]:
    """Split a captured SSE body into frames.

    Only a line that *begins* with "event:" or "data:" carries framing;
    the same tokens inside a JSON payload (e.g. '"text":"event: error"')
    never open a frame. One leading blank is stripped per SSE convention;
    comment (":...") and unknown lines are ignored. Multiple "data:" lines
    in one block join with "\\n".
    """
    frames: list[dict] = []
    for block in re.split(r"\r?\n\r?\n", text):
        if not block.strip():
            continue
        event: str | None = None
        datas: list[str] = []
        for line in block.splitlines():
            if line.startswith("event:"):
                rest = line[6:]
                event = rest[1:] if rest.startswith(" ") else rest
            elif line.startswith("data:"):
                rest = line[5:]
                datas.append(rest[1:] if rest.startswith(" ") else rest)
            elif line.startswith(":") or not line.strip():
                continue
            else:
                # Stray line inside a block: never framing, never data.
                continue
        if event is None and not datas:
            continue
        frames.append({"event": event, "data": "\n".join(datas)})
        if len(frames) >= max_frames:
            break
    return frames


def sse_token(frame: dict) -> str:
    """Comparison token: event name, or "[DONE]"/"data" for data-only frames."""
    if frame["event"] is not None:
        return frame["event"]
    return "[DONE]" if frame["data"] == "[DONE]" else "data"


def _json_frames(frames: list[dict]) -> tuple[list[dict], list[str]]:
    """Decode JSON data frames; returns (payloads, errors)."""
    payloads: list[dict] = []
    errors: list[str] = []
    for frame in frames:
        if frame["data"] in ("[DONE]", ""):
            continue
        try:
            payloads.append(json.loads(frame["data"]))
        except ValueError:
            errors.append(f"non-JSON SSE data frame: {frame['data'][:120]!r}")
    return payloads, errors


def check_responses_integrity(frames: list[dict], where: str) -> list[str]:
    """Identity/terminal relationships across a Responses event stream."""
    failures: list[str] = []
    payloads, errors = _json_frames(frames)
    failures += [f"{where}: {e}" for e in errors]
    if not payloads:
        return failures + [f"{where}: empty Responses stream"]
    types = [p.get("type") if isinstance(p, dict) else None for p in payloads]
    if types[0] != "response.created":
        failures.append(f"{where}: first event {types[0]!r}, want 'response.created'")
    if types[-1] != "response.completed":
        failures.append(f"{where}: last event {types[-1]!r}, want 'response.completed'")
    added: dict[int, str] = {}      # output_index -> item id
    added_call: dict[str, str] = {}  # item id -> call_id (function calls)
    added_pos: dict[str, int] = {}
    deltas: dict[str, list[str]] = {}
    done_text: dict[str, str] = {}
    done_pos: dict[str, int] = {}
    item_done_ids: set[str] = set()
    for pos, p in enumerate(payloads):
        if not isinstance(p, dict):
            failures.append(f"{where}: frame {pos} not a JSON object")
            continue
        kind = p.get("type")
        if kind == "response.output_item.added":
            item = p.get("item") or {}
            idx, iid = p.get("output_index"), item.get("id")
            if type(idx) is not int or not isinstance(iid, str) or not iid:
                failures.append(f"{where}: added item without index/id at {pos}")
            elif idx in added:
                failures.append(f"{where}: duplicate added output_index {idx}")
            else:
                added[idx] = iid
                added_pos[iid] = pos
                deltas[iid] = []
                if isinstance(item.get("call_id"), str):
                    added_call[iid] = item["call_id"]
        elif kind in ("response.output_text.delta",
                      "response.function_call_arguments.delta"):
            iid = p.get("item_id")
            if iid not in deltas:
                failures.append(f"{where}: delta for unknown item {iid!r} at {pos}")
            else:
                if pos < added_pos[iid]:
                    failures.append(f"{where}: delta before added for {iid!r}")
                if p.get("output_index") not in added:
                    failures.append(f"{where}: delta with unknown output_index at {pos}")
                frag = p.get("delta")
                if not isinstance(frag, str):
                    failures.append(f"{where}: non-string delta at {pos}")
                else:
                    deltas[iid].append(frag)
        elif kind in ("response.output_text.done",
                      "response.function_call_arguments.done"):
            iid = p.get("item_id")
            if iid not in deltas:
                failures.append(f"{where}: done for unknown item {iid!r} at {pos}")
            else:
                if pos < added_pos[iid]:
                    failures.append(f"{where}: done before added for {iid!r}")
                key = "text" if kind == "response.output_text.done" else "arguments"
                done_text[iid] = p.get(key)
                done_pos[iid] = pos
                if not deltas[iid]:
                    pass  # empty tool args / empty text: nothing to reassemble
                elif done_text[iid] != "".join(deltas[iid]):
                    failures.append(
                        f"{where}: done {key} != joined deltas for {iid!r}")
        elif kind == "response.output_item.done":
            item = p.get("item") or {}
            iid = item.get("id")
            if iid not in deltas:
                failures.append(f"{where}: item.done for unknown item {iid!r} at {pos}")
            else:
                item_done_ids.add(iid)
                if iid in done_pos and pos < done_pos[iid]:
                    failures.append(f"{where}: item.done before done for {iid!r}")
                if item.get("type") == "function_call":
                    if (item.get("call_id") != added_call.get(iid)
                            or item.get("arguments") != done_text.get(iid)):
                        failures.append(
                            f"{where}: item.done changes identity/args for {iid!r}")
                elif item.get("type") == "message":
                    try:
                        text = item["content"][0].get("text")
                    except (KeyError, IndexError, TypeError, AttributeError):
                        text = None
                    if text != done_text.get(iid):
                        failures.append(
                            f"{where}: item.done changes text for {iid!r}")
    comp = payloads[-1] if isinstance(payloads[-1], dict) else {}
    if comp.get("type") == "response.completed":
        resp_ids = set()
        for p in payloads:
            if isinstance(p, dict) and p.get("type") in (
                    "response.created", "response.in_progress", "response.completed"):
                rid = (p.get("response") or {}).get("id")
                if isinstance(rid, str) and rid:
                    resp_ids.add(rid)
        if len(resp_ids) > 1:
            failures.append(f"{where}: response id changes across lifecycle: {sorted(resp_ids)!r}")
        output = (comp.get("response") or {}).get("output") or []
        ids = [i.get("id") for i in output if isinstance(i, dict)]
        if set(ids) != set(added.values()):
            failures.append(
                f"{where}: completed output ids {ids!r} != added {sorted(added.values())!r}")
        for item in output:
            if not isinstance(item, dict):
                continue
            iid = item.get("id")
            if item.get("type") == "function_call" and iid in deltas:
                if (item.get("arguments") != done_text.get(iid)
                        or item.get("call_id") != added_call.get(iid)):
                    failures.append(
                        f"{where}: completed repeats different args/identity for {iid!r}")
            elif item.get("type") == "message" and iid in deltas:
                try:
                    text = item["content"][0].get("text")
                except (KeyError, IndexError, TypeError, AttributeError):
                    text = None
                if text != done_text.get(iid):
                    failures.append(
                        f"{where}: completed repeats different text for {iid!r}")
    return failures


def check_chat_integrity(frames: list[dict], where: str,
                         joined_tool_args: dict | None = None) -> list[str]:
    """Chunk-ID stability, tool-call identity and terminal reason (Chat)."""
    failures: list[str] = []
    if not frames or frames[-1]["data"] != "[DONE]":
        return failures + [f"{where}: Chat stream missing terminal [DONE]"]
    chunks, errors = _json_frames(frames)
    failures += [f"{where}: {e}" for e in errors]
    if not chunks:
        return failures + [f"{where}: empty Chat stream"]
    ids = {c.get("id") for c in chunks if isinstance(c, dict)}
    if len(ids) != 1 or not next(iter(ids)):
        failures.append(f"{where}: chunk ids not stable: {sorted(map(str, ids))!r}")
    finish = None
    tool_ids: dict[int, str] = {}
    tool_names: dict[int, str] = {}
    arg_frags: dict[int, list[str]] = {}
    for pos, c in enumerate(chunks):
        if not isinstance(c, dict):
            failures.append(f"{where}: chunk {pos} not a JSON object")
            continue
        for choice in c.get("choices") or []:
            if choice.get("finish_reason") is not None:
                finish = choice.get("finish_reason")
            for call in (choice.get("delta") or {}).get("tool_calls") or []:
                idx = call.get("index")
                if type(idx) is not int:
                    failures.append(f"{where}: tool_call without int index at {pos}")
                    continue
                if call.get("id"):
                    if idx in tool_ids and tool_ids[idx] != call["id"]:
                        failures.append(f"{where}: tool_call id changed at index {idx}")
                    tool_ids[idx] = call["id"]
                name = (call.get("function") or {}).get("name")
                if name:
                    if idx in tool_names and tool_names[idx] != name:
                        failures.append(f"{where}: tool_call name changed at index {idx}")
                    tool_names[idx] = name
                arg_frags.setdefault(idx, []).append(
                    (call.get("function") or {}).get("arguments", ""))
    if finish is None:
        failures.append(f"{where}: Chat stream missing terminal finish_reason")
    for idx, frags in arg_frags.items():
        joined = "".join(f or "" for f in frags)
        if joined and idx in tool_ids:
            try:
                json.loads(joined)
            except ValueError:
                failures.append(f"{where}: tool_call args not JSON at index {idx}")
    for idx, want in (joined_tool_args or {}).items():
        try:
            key = int(idx)
        except (TypeError, ValueError):
            failures.append(f"{where}: bad joined_tool_args index {idx!r}")
            continue
        got = "".join(f or "" for f in arg_frags.get(key, []))
        if got != want:
            failures.append(
                f"{where}: tool_call args at index {key} reassemble to "
                f"{got[:80]!r}, want {want[:80]!r}")
    return failures


_ANTHROPIC_TOP_KEYS = {
    "message_start": {"type", "message"},
    "content_block_start": {"type", "index", "content_block"},
    "content_block_delta": {"type", "index", "delta"},
    "content_block_stop": {"type", "index"},
    "message_delta": {"type", "delta", "usage"},
    "message_stop": {"type"},
    "ping": {"type"},
    "error": {"type", "error"},
}

_ANTHROPIC_STOPS = {"end_turn", "max_tokens", "stop_sequence", "tool_use"}

_ANTHROPIC_COUNTERS = ("input_tokens", "output_tokens",
                       "cache_creation_input_tokens", "cache_read_input_tokens")

_ANTHROPIC_MESSAGE_KEYS = {"id", "type", "role", "model", "content",
                           "stop_reason", "stop_sequence", "usage"}

_ANTHROPIC_BLOCK_KEYS = {"type", "id", "name", "input", "text", "citations"}


def _anthropic_count(value) -> bool:
    """Non-negative int counter (bool is never a valid count)."""
    return type(value) is int and value >= 0


def _check_anthropic_usage(obj, what: str, where: str, pos: int,
                           failures: list[str],
                           require_input: bool, require_output: bool) -> None:
    """Validate an Anthropic usage object without echoing its values."""
    if not isinstance(obj, dict):
        failures.append(f"{where}: {what} usage must be an object at {pos}")
        return
    for key, val in obj.items():
        if key in _ANTHROPIC_COUNTERS:
            if val is None:
                continue  # absent
            if not _anthropic_count(val):
                failures.append(
                    f"{where}: {what} usage.{key} must be a non-negative int "
                    f"at {pos}")
        elif val is None:
            continue  # documented nulls, or forward-compatible absence
        else:
            failures.append(
                f"{where}: unexpected {what} usage field {key!r} at {pos}")
    if require_input and not _anthropic_count(obj.get("input_tokens")):
        failures.append(
            f"{where}: {what} usage.input_tokens must be a non-negative int "
            f"at {pos}")
    if require_output and not _anthropic_count(obj.get("output_tokens")):
        failures.append(
            f"{where}: {what} usage.output_tokens must be a non-negative int "
            f"at {pos}")


def check_anthropic_integrity(frames: list[dict], where: str,
                              joined_tool_args: dict | None = None) -> list[str]:
    """Block lifecycle/identity and terminal usage across an Anthropic stream.

    Success streams open once with message_start and close once with
    message_stop; the single message_delta between them carries the
    authoritative usage and stop_reason. Each content_block_start opens one
    unique non-negative index carrying a text or tool_use block; deltas and
    stops must reference a started, unstopped index with a matching delta
    type; tool_use ids are unique and their fragments reassemble to one JSON
    object. Empty text blocks and fully seeded tool inputs need no deltas.
    """
    failures: list[str] = []
    if not frames:
        return [f"{where}: empty Anthropic stream"]
    tokens = [sse_token(f) for f in frames]
    if "[DONE]" in tokens:
        failures.append(f"{where}: Anthropic stream must not contain data: [DONE]")
    # Decode each frame in place so framing and payload stay aligned: the
    # event label must equal the payload type, and only line-start event:
    # tokens parsed by parse_sse count as framing (model text mentioning
    # "event:" inside a JSON string never does).
    payloads: list[dict | None] = []
    for pos, frame in enumerate(frames):
        event, data = frame.get("event"), frame.get("data", "")
        if data == "[DONE]":
            payloads.append(None)
            continue
        if data == "":
            if event == "ping":
                payloads.append({"type": "ping"})
                continue
            failures.append(f"{where}: empty SSE data frame at {pos}")
            payloads.append(None)
            continue
        try:
            payload = json.loads(data)
        except ValueError:
            failures.append(f"{where}: non-JSON SSE data frame at {pos}")
            payloads.append(None)
            continue
        if not isinstance(payload, dict):
            failures.append(f"{where}: frame {pos} not a JSON object")
            payloads.append(None)
            continue
        payloads.append(payload)
        ptype = payload.get("type")
        if event is None or not isinstance(ptype, str) or event != ptype:
            failures.append(f"{where}: event/type mismatch at {pos}")
            payloads[-1] = None
        elif ptype not in _ANTHROPIC_TOP_KEYS:
            failures.append(f"{where}: unknown Anthropic event at {pos}")
            payloads[-1] = None
    if not any(p is not None for p in payloads):
        return failures + [f"{where}: empty Anthropic stream"]
    live = [(pos, p) for pos, p in enumerate(payloads)
            if p is not None and p.get("type") != "ping"]
    if not live:
        return failures + [f"{where}: empty Anthropic stream"]
    if live[0][1].get("type") != "message_start":
        failures.append(
            f"{where}: first event {live[0][1].get('type')!r}, "
            "want 'message_start'")
    if live[-1][1].get("type") != "message_stop":
        failures.append(
            f"{where}: last event {live[-1][1].get('type')!r}, "
            "want 'message_stop'")
    started: dict[int, str] = {}   # index -> "text" | "tool_use"
    stopped: set[int] = set()
    text_frags: dict[int, list[str]] = {}
    arg_frags: dict[int, list[str]] = {}
    arg_seed: dict[int, str] = {}
    seen_tool_ids: set[str] = set()
    start_count = stop_count = delta_count = 0
    delta_pos = -1
    max_content_pos = -1
    for pos, p in live:
        kind = p.get("type")
        extra = set(p) - _ANTHROPIC_TOP_KEYS[kind]
        if extra:
            failures.append(
                f"{where}: unexpected {kind} field "
                f"{sorted(extra)[0]!r} at {pos}")
            continue
        if kind == "message_start":
            start_count += 1
            msg = p.get("message")
            if not isinstance(msg, dict):
                failures.append(
                    f"{where}: message_start without message at {pos}")
                continue
            if set(msg) - _ANTHROPIC_MESSAGE_KEYS:
                failures.append(
                    f"{where}: unexpected message_start.message field "
                    f"{sorted(set(msg) - _ANTHROPIC_MESSAGE_KEYS)[0]!r} "
                    f"at {pos}")
                continue
            ident = msg.get("id")
            if not isinstance(ident, str) or not ident.strip():
                failures.append(
                    f"{where}: message_start without message id at {pos}")
            for key, want in (("type", "message"), ("role", "assistant")):
                if msg.get(key) is not None and msg.get(key) != want:
                    failures.append(
                        f"{where}: message_start message {key} must be "
                        f"{want!r} at {pos}")
            model = msg.get("model")
            if model is not None and (
                    not isinstance(model, str) or not model.strip()):
                failures.append(
                    f"{where}: message_start without model at {pos}")
            content = msg.get("content")
            if content is not None and content != []:
                failures.append(
                    f"{where}: message_start content must be empty at {pos}")
            for key in ("stop_reason", "stop_sequence"):
                if msg.get(key) is not None:
                    failures.append(
                        f"{where}: message_start {key} must be null "
                        f"at {pos}")
            _check_anthropic_usage(msg.get("usage"), "message_start", where,
                                   pos, failures, True, False)
        elif kind == "content_block_start":
            max_content_pos = pos
            idx = p.get("index")
            if not _anthropic_count(idx):
                failures.append(
                    f"{where}: content_block_start without valid index "
                    f"at {pos}")
                continue
            if idx in started:
                failures.append(
                    f"{where}: duplicate content_block_start index "
                    f"at {pos}")
                continue
            block = p.get("content_block")
            if not isinstance(block, dict):
                failures.append(
                    f"{where}: content_block_start without content block "
                    f"at {pos}")
                continue
            if set(block) - _ANTHROPIC_BLOCK_KEYS:
                failures.append(
                    f"{where}: unexpected content_block field "
                    f"{sorted(set(block) - _ANTHROPIC_BLOCK_KEYS)[0]!r} "
                    f"at {pos}")
                continue
            btype = block.get("type")
            if btype == "text":
                text = block.get("text")
                if text is None:
                    text = ""
                if not isinstance(text, str):
                    failures.append(
                        f"{where}: text block without text at {pos}")
                    continue
                if any(block.get(k) is not None
                       for k in ("id", "name", "input")):
                    failures.append(
                        f"{where}: text block carries tool fields at {pos}")
                    continue
                if block.get("citations") is not None and block.get(
                        "citations") != []:
                    failures.append(
                        f"{where}: text block carries citations at {pos}")
                    continue
                started[idx] = "text"
                text_frags[idx] = []
            elif btype == "tool_use":
                ident, name = block.get("id"), block.get("name")
                if not isinstance(ident, str) or not ident.strip():
                    failures.append(
                        f"{where}: tool_use block without id at {pos}")
                    continue
                if ident in seen_tool_ids:
                    failures.append(
                        f"{where}: duplicate tool_use id at {pos}")
                    continue
                if not isinstance(name, str) or not name.strip():
                    failures.append(
                        f"{where}: tool_use block without name at {pos}")
                    continue
                seed = block.get("input")
                if seed is not None and not isinstance(seed, dict):
                    failures.append(
                        f"{where}: tool_use input must be an object "
                        f"at {pos}")
                    continue
                if block.get("text") is not None:
                    failures.append(
                        f"{where}: tool_use block carries text at {pos}")
                    continue
                seen_tool_ids.add(ident)
                started[idx] = "tool_use"
                arg_frags[idx] = []
                if isinstance(seed, dict):
                    try:
                        compact = json.dumps(seed, separators=(",", ":"))
                    except (TypeError, ValueError):
                        failures.append(
                            f"{where}: tool_use input not serializable "
                            f"at {pos}")
                        continue
                    arg_seed[idx] = "" if compact == "{}" else compact
                else:
                    arg_seed[idx] = ""
            else:
                failures.append(
                    f"{where}: content_block_start has unmapped block type "
                    f"at {pos}")
        elif kind == "content_block_delta":
            max_content_pos = pos
            idx = p.get("index")
            if not _anthropic_count(idx) or idx not in started:
                failures.append(
                    f"{where}: delta for unknown block index at {pos}")
                continue
            if idx in stopped:
                failures.append(
                    f"{where}: delta after stop for index at {pos}")
                continue
            delta = p.get("delta")
            if not isinstance(delta, dict):
                failures.append(
                    f"{where}: content_block_delta without delta at {pos}")
                continue
            if set(delta) - {"type", "text", "partial_json"}:
                failures.append(
                    f"{where}: unexpected content_block_delta.delta field "
                    f"{sorted(set(delta) - {'type', 'text', 'partial_json'})[0]!r} "
                    f"at {pos}")
                continue
            dtype = delta.get("type")
            if dtype == "text_delta":
                if started[idx] != "text":
                    failures.append(
                        f"{where}: text_delta for non-text block at {pos}")
                    continue
                if not isinstance(delta.get("text"), str):
                    failures.append(
                        f"{where}: text_delta without text at {pos}")
                    continue
                text_frags[idx].append(delta["text"])
            elif dtype == "input_json_delta":
                if started[idx] != "tool_use":
                    failures.append(
                        f"{where}: input_json_delta for non-tool block "
                        f"at {pos}")
                    continue
                if not isinstance(delta.get("partial_json"), str):
                    failures.append(
                        f"{where}: input_json_delta without partial_json "
                        f"at {pos}")
                    continue
                arg_frags[idx].append(delta["partial_json"])
            else:
                failures.append(f"{where}: unknown delta type at {pos}")
        elif kind == "content_block_stop":
            max_content_pos = pos
            idx = p.get("index")
            if not _anthropic_count(idx) or idx not in started:
                failures.append(
                    f"{where}: stop for unknown block index at {pos}")
            elif idx in stopped:
                failures.append(f"{where}: duplicate stop for index at {pos}")
            else:
                stopped.add(idx)
        elif kind == "message_delta":
            delta_count += 1
            delta_pos = pos
            delta = p.get("delta")
            if not isinstance(delta, dict):
                failures.append(
                    f"{where}: message_delta without delta at {pos}")
                continue
            if set(delta) - {"stop_reason", "stop_sequence"}:
                failures.append(
                    f"{where}: unexpected message_delta.delta field "
                    f"{sorted(set(delta) - {'stop_reason', 'stop_sequence'})[0]!r} "
                    f"at {pos}")
                continue
            stop = delta.get("stop_reason")
            if not isinstance(stop, str) or not stop:
                failures.append(
                    f"{where}: message_delta without stop_reason at {pos}")
            elif stop not in _ANTHROPIC_STOPS:
                failures.append(
                    f"{where}: message_delta has unmapped stop_reason "
                    f"at {pos}")
            seq = delta.get("stop_sequence")
            if seq is not None and not isinstance(seq, str):
                failures.append(
                    f"{where}: message_delta stop_sequence must be null "
                    f"or text at {pos}")
            _check_anthropic_usage(p.get("usage"), "message_delta", where,
                                   pos, failures, False, True)
        elif kind == "message_stop":
            stop_count += 1
        elif kind == "error":
            failures.append(
                f"{where}: success stream must not contain error at {pos}")
    if start_count == 0:
        failures.append(f"{where}: stream missing message_start")
    elif start_count > 1:
        failures.append(f"{where}: duplicate message_start")
    if stop_count == 0:
        failures.append(f"{where}: stream missing message_stop")
    elif stop_count > 1:
        failures.append(f"{where}: duplicate message_stop")
    if delta_count == 0:
        failures.append(
            f"{where}: stream missing message_delta with terminal usage")
    elif delta_count > 1:
        failures.append(f"{where}: duplicate message_delta")
    if delta_count == 1 and max_content_pos > delta_pos:
        failures.append(f"{where}: content after message_delta")
    if set(started) != stopped:
        failures.append(
            f"{where}: blocks started {sorted(started)!r} != stopped "
            f"{sorted(stopped)!r}")
    for idx, kind in started.items():
        if kind != "tool_use":
            continue
        assembled = arg_seed.get(idx, "") + "".join(arg_frags.get(idx, []))
        if assembled in ("", "{}"):
            continue  # valid empty tool input needs no deltas
        try:
            parsed = json.loads(assembled)
        except ValueError:
            failures.append(
                f"{where}: tool_call args not a JSON object at index {idx}")
            continue
        if not isinstance(parsed, dict):
            failures.append(
                f"{where}: tool_call args not a JSON object at index {idx}")
    for index, expected in (joined_tool_args or {}).items():
        try:
            idx = int(index)
        except (ValueError, TypeError):
            failures.append(f"{where}: invalid expected tool argument index")
            continue
        assembled = arg_seed.get(idx, "") + "".join(arg_frags.get(idx, []))
        if started.get(idx) != "tool_use" or (assembled or "{}") != expected:
            failures.append(f"{where}: joined tool arguments differ at index {idx}")
    return failures


def _gemini_count(value) -> bool:
    """Non-negative int counter (bool is never a valid count)."""
    return type(value) is int and value >= 0


_GEMINI_TOP_KEYS = {"candidates", "promptFeedback", "usageMetadata",
                    "modelVersion", "responseId"}

_GEMINI_CAND_KEYS = {"content", "finishReason", "finishMessage",
                     "safetyRatings", "citationMetadata", "groundingMetadata",
                     "avgLogprobs", "logprobsResult", "index",
                     "tokenCountMetadata", "continuationToken"}

_GEMINI_PART_KEYS = {"text", "functionCall", "functionResponse",
                     "thought", "thoughtSignature", "inlineData", "fileData",
                     "executableCode", "codeExecutionResult", "toolCall",
                     "toolResponse", "mediaResolution", "videoMetadata",
                     "partMetadata", "mediaProcessing"}

_GEMINI_FINISHES = {"STOP", "MAX_TOKENS", "SAFETY", "RECITATION",
                    "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
                    "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT",
                    "IMAGE_RECITATION", "IMAGE_OTHER", "LANGUAGE",
                    "NO_IMAGE", "MALFORMED_FUNCTION_CALL"}

_GEMINI_FILTER_FINISHES = {"SAFETY", "RECITATION", "BLOCKLIST",
                           "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY",
                           "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION",
                           "IMAGE_OTHER", "LANGUAGE", "NO_IMAGE"}

_GEMINI_USAGE_KEYS = {"promptTokenCount", "candidatesTokenCount",
                      "totalTokenCount", "cachedContentTokenCount",
                      "thoughtsTokenCount", "toolUsePromptTokenCount",
                      "promptTokensDetails", "cacheTokensDetails",
                      "candidatesTokensDetails", "toolUsePromptTokensDetails",
                      "trafficType"}


def _check_gemini_usage(obj, where: str, pos, failures: list[str]) -> tuple[int, int, int, int] | None:
    """Validate one Gemini usageMetadata object; returns prompt/cand/cached+thought/tooluse parts.

    Returns (prompt, candidates, cached, extra) where extra is
    thoughts+tool-use, or None when the object is malformed. Totals use
    exact integer arithmetic: total == prompt+candidates+thoughts+tool-use,
    and the cached share is a subset of prompt. Unknown non-null members
    fail closed; documented nulls read as absent.
    """
    if not isinstance(obj, dict):
        failures.append(f"{where}: usageMetadata must be an object at {pos}")
        return None
    for key, val in obj.items():
        if key in _GEMINI_USAGE_KEYS:
            continue
        if val is None:
            continue  # forward-compatible absent default
        failures.append(
            f"{where}: unexpected usageMetadata field {key!r} at {pos}")
        return None
    counts: dict[str, int] = {}
    for key in ("promptTokenCount", "candidatesTokenCount", "totalTokenCount"):
        val = obj.get(key)
        if not _gemini_count(val):
            failures.append(
                f"{where}: usageMetadata.{key} must be a non-negative int "
                f"at {pos}")
            return None
        counts[key] = val
    cached = thoughts = tooluse = 0
    for key, slot in (("cachedContentTokenCount", "cached"),
                      ("thoughtsTokenCount", "thoughts"),
                      ("toolUsePromptTokenCount", "tooluse")):
        val = obj.get(key)
        if val is None:
            continue
        if not _gemini_count(val):
            failures.append(
                f"{where}: usageMetadata.{key} must be a non-negative int "
                f"at {pos}")
            return None
        if slot == "cached":
            cached = val
        elif slot == "thoughts":
            thoughts = val
        else:
            tooluse = val
    for key in ("promptTokensDetails", "cacheTokensDetails",
                "candidatesTokensDetails", "toolUsePromptTokensDetails"):
        val = obj.get(key)
        if val is None:
            continue
        if not isinstance(val, list) or len(val) > 0:
            failures.append(
                f"{where}: usageMetadata.{key} has no Gemini representation "
                f"at {pos}")
            return None
    traffic = obj.get("trafficType")
    if traffic is not None and (
            not isinstance(traffic, str) or not traffic):
        failures.append(
            f"{where}: usageMetadata.trafficType must be text at {pos}")
        return None
    prompt, cand, total = (counts["promptTokenCount"],
                           counts["candidatesTokenCount"],
                           counts["totalTokenCount"])
    if cached > prompt:
        failures.append(
            f"{where}: usageMetadata cached share exceeds prompt at {pos}")
        return None
    want = prompt + cand + thoughts + tooluse
    if want < prompt or want < cand or total != want:
        failures.append(
            f"{where}: usageMetadata total must equal "
            f"prompt+candidates+thoughts+tool-use at {pos}")
        return None
    return prompt, cand, cached, thoughts + tooluse


def check_gemini_integrity(frames: list[dict], where: str,
                            joined_args: dict | None = None,
                            joined_text: str | None = None) -> list[str]:
    """Object/identity/terminal relationships across a Gemini stream.

    Each data-only SSE frame (or decoded JSON-array element) is one partial
    GenerateContentResponse: candidates carry content parts (text plus
    complete functionCall objects) with candidate index 0 and role model;
    modelVersion/responseId repeat with a constant value when present;
    finishReason appears exactly once, on the last frame, with a reviewed
    value; the terminal frame carries the authoritative usageMetadata whose
    total equals prompt+candidates+thoughts+tool-use. Empty text parts and
    empty-args calls ({}) are valid. Success streams never contain
    data: [DONE], event names, or unknown non-null semantics. Malformed
    shapes are failures, never crashes: every access is type-checked.
    """
    failures: list[str] = []
    if not frames:
        return [f"{where}: empty Gemini stream"]
    for pos, frame in enumerate(frames):
        if frame.get("event") is not None:
            failures.append(
                f"{where}: Gemini stream must be data-only at {pos}")
        if frame.get("data") == "[DONE]":
            failures.append(
                f"{where}: Gemini stream must not contain data: [DONE]")
    payloads: list[dict | None] = []
    for pos, frame in enumerate(frames):
        data = frame.get("data", "")
        if data in ("[DONE]", ""):
            payloads.append(None)
            continue
        try:
            payload = json.loads(data)
        except ValueError:
            failures.append(f"{where}: non-JSON SSE data frame at {pos}")
            payloads.append(None)
            continue
        if not isinstance(payload, dict):
            failures.append(f"{where}: frame {pos} not a JSON object")
            payloads.append(None)
            continue
        payloads.append(payload)
    live = [(pos, p) for pos, p in enumerate(payloads) if p is not None]
    if not live:
        return failures + [f"{where}: empty Gemini stream"]
    versions: set[str] = set()
    responses: set[str] = set()
    texts: list[str] = []
    calls: list[tuple[str | None, str, dict]] = []
    seen_call_ids: set[str] = set()
    finish_pos: list[int] = []
    finish_value: str | None = None
    blocked = False
    for pos, p in live:
        assert isinstance(p, dict)
        for key, val in p.items():
            if key in _GEMINI_TOP_KEYS:
                continue
            if val is None:
                continue  # forward-compatible absent default
            failures.append(
                f"{where}: unexpected GenerateContentResponse field "
                f"{key!r} at {pos}")
            payloads[pos] = None
            break
        if payloads[pos] is None:
            continue
        for key in ("modelVersion", "responseId"):
            val = p.get(key)
            if val is None:
                continue
            if not isinstance(val, str) or not val:
                failures.append(
                    f"{where}: {key} must be a non-empty string at {pos}")
            elif key == "modelVersion":
                versions.add(val)
            else:
                responses.add(val)
        raw_pf = p.get("promptFeedback")
        if isinstance(raw_pf, dict):
            block = raw_pf.get("blockReason")
            if block is not None:
                if not isinstance(block, str) or not block:
                    failures.append(
                        f"{where}: promptFeedback.blockReason must be text "
                        f"at {pos}")
                else:
                    blocked = True
        raw_cands = p.get("candidates")
        if raw_cands is None:
            continue  # candidacy-less chunk: only valid as a blocked signal
        if not isinstance(raw_cands, list) or len(raw_cands) != 1:
            failures.append(
                f"{where}: candidates must hold exactly one choice "
                f"at {pos}")
            continue
        cand = raw_cands[0]
        if not isinstance(cand, dict):
            failures.append(f"{where}: candidate not an object at {pos}")
            continue
        for key, val in cand.items():
            if key in _GEMINI_CAND_KEYS:
                continue
            if val is None:
                continue
            failures.append(
                f"{where}: unexpected candidate field {key!r} at {pos}")
            cand = None  # type: ignore[assignment]
            break
        if cand is None:
            continue
        raw_idx = cand.get("index")
        if raw_idx is not None and raw_idx != 0:
            failures.append(
                f"{where}: candidate index must be 0 at {pos}")
        raw_finish = cand.get("finishReason")
        if raw_finish is not None:
            if not isinstance(raw_finish, str) or not raw_finish:
                failures.append(
                    f"{where}: finishReason must be text at {pos}")
            else:
                finish_pos.append(pos)
                finish_value = raw_finish
                if raw_finish not in _GEMINI_FINISHES:
                    failures.append(
                        f"{where}: finishReason {raw_finish!r} has no Chat "
                        f"representation at {pos}")
        raw_content = cand.get("content")
        if raw_content is None:
            continue
        if not isinstance(raw_content, dict):
            failures.append(
                f"{where}: content must be an object at {pos}")
            continue
        raw_role = raw_content.get("role")
        if raw_role is not None and raw_role != "model":
            failures.append(
                f"{where}: content role must be model at {pos}")
        raw_parts = raw_content.get("parts")
        if raw_parts is None:
            failures.append(f"{where}: content without parts at {pos}")
            continue
        if not isinstance(raw_parts, list):
            failures.append(f"{where}: parts must be an array at {pos}")
            continue
        for pi, part in enumerate(raw_parts):
            if not isinstance(part, dict):
                failures.append(
                    f"{where}: part {pi} not an object at {pos}")
                continue
            for key, val in part.items():
                if key in _GEMINI_PART_KEYS:
                    continue
                if val is None:
                    continue
                failures.append(
                    f"{where}: unexpected part field {key!r} at {pos}")
            raw_text = part.get("text")
            if raw_text is not None:
                if not isinstance(raw_text, str):
                    failures.append(
                        f"{where}: part text must be text at {pos}")
                elif raw_text:
                    texts.append(raw_text)
            for key in ("thoughtSignature", "inlineData", "fileData",
                        "executableCode", "codeExecutionResult", "toolCall",
                        "toolResponse", "functionResponse",
                        "mediaResolution", "videoMetadata", "partMetadata",
                        "mediaProcessing"):
                val = part.get(key)
                if val is not None:
                    failures.append(
                        f"{where}: part field {key!r} has no Chat "
                        f"representation at {pos}")
            raw_thought = part.get("thought")
            if raw_thought is True:
                failures.append(
                    f"{where}: thought parts have no Chat representation "
                    f"at {pos}")
            elif raw_thought is not None and raw_thought is not False:
                failures.append(
                    f"{where}: part thought must be bool at {pos}")
            raw_fc = part.get("functionCall")
            if raw_fc is None:
                continue
            if not isinstance(raw_fc, dict):
                failures.append(
                    f"{where}: functionCall must be an object at {pos}")
                continue
            for key, val in raw_fc.items():
                if key in ("name", "args", "id"):
                    continue
                if val is None:
                    continue
                failures.append(
                    f"{where}: unexpected functionCall field {key!r} "
                    f"at {pos}")
            name = raw_fc.get("name")
            if not isinstance(name, str) or not name.strip():
                failures.append(
                    f"{where}: functionCall without name at {pos}")
                continue
            call_id = raw_fc.get("id")
            if call_id is not None:
                if not isinstance(call_id, str) or not call_id.strip():
                    failures.append(
                        f"{where}: functionCall id must be text at {pos}")
                    continue
                if call_id in seen_call_ids:
                    failures.append(
                        f"{where}: duplicate functionCall id at {pos}")
                    continue
                seen_call_ids.add(call_id)
            raw_args = raw_fc.get("args")
            if raw_args is None:
                args: dict = {}
            elif not isinstance(raw_args, dict):
                failures.append(
                    f"{where}: functionCall args not a JSON object "
                    f"at {pos}")
                continue
            else:
                args = raw_args
            calls.append((call_id, name, args))
        raw_usage = p.get("usageMetadata")
        if raw_usage is not None:
            _check_gemini_usage(raw_usage, where, pos, failures)
    if len(versions) > 1:
        failures.append(f"{where}: modelVersion changes across the stream")
    if len(responses) > 1:
        failures.append(f"{where}: responseId changes across the stream")
    if not finish_pos:
        failures.append(f"{where}: stream missing terminal finishReason")
    elif len(finish_pos) > 1:
        failures.append(f"{where}: duplicate finishReason")
    elif finish_pos[0] != live[-1][0]:
        failures.append(
            f"{where}: finishReason only on the terminal frame")
    if blocked and finish_value == "STOP":
        failures.append(
            f"{where}: blocked prompt must never report ordinary STOP")
    last_pos = live[-1][0]
    last = live[-1][1]
    assert isinstance(last, dict)
    if not isinstance(last.get("usageMetadata"), dict):
        failures.append(
            f"{where}: terminal usageMetadata is authoritative and required")
    if joined_text is not None and "".join(texts) != joined_text:
        failures.append(f"{where}: joined text differs")
    for index, expected in (joined_args or {}).items():
        try:
            idx = int(index)
        except (ValueError, TypeError):
            failures.append(f"{where}: invalid expected tool argument index")
            continue
        if not (0 <= idx < len(calls)):
            failures.append(
                f"{where}: joined tool arguments missing index {idx}")
            continue
        want = expected
        if isinstance(want, str):
            try:
                want = json.loads(want)
            except ValueError:
                failures.append(
                    f"{where}: expected tool arguments not JSON at "
                    f"index {idx}")
                continue
        if calls[idx][2] != want:
            failures.append(
                f"{where}: joined tool arguments differ at index {idx}")
    return failures


def check_gemini_array_body(text: str, where: str,
                            joined_args: dict | None = None,
                            joined_text: str | None = None) -> list[str]:
    """Validate a streamed Gemini JSON-array body without crashing.

    Framing first: the body must open with `[` and close with `]` (EOF or
    a trailing comma before `]` is truncation, never a valid success). A
    body that fails to decode is a failure, never a valid closed array.
    Elements then face the same object/identity/terminal rules as the SSE
    validator, so text and complete functionCall args reassemble
    independently of chunk boundaries.
    """
    failures: list[str] = []
    if "[DONE]" in text:
        failures.append(f"{where}: Gemini array must not contain [DONE]")
        return failures
    stripped = text.strip()
    if not stripped.startswith("["):
        return failures + [f"{where}: Gemini array must open with '['"]
    if not stripped.endswith("]"):
        return failures + [f"{where}: truncated Gemini array (missing ']')"]
    try:
        elements = json.loads(stripped)
    except ValueError:
        return failures + [f"{where}: invalid Gemini array (not closed JSON)"]
    if not isinstance(elements, list):
        return failures + [f"{where}: Gemini array must be a JSON array"]
    if not elements:
        return failures + [f"{where}: empty Gemini stream"]
    frames = [{"event": None, "data": json.dumps(el)} for el in elements]
    return failures + check_gemini_integrity(frames, where, joined_args,
                                             joined_text)


def check_sse_frames(frames: list[dict], spec: dict, where: str) -> list[str]:
    """Evaluate an expect.sse block against parsed frames."""
    failures: list[str] = []
    tokens = [sse_token(f) for f in frames]
    if "events" in spec and tokens != spec["events"]:
        failures.append(f"{where}: SSE events {tokens!r}, want {spec['events']!r}")
    for tok in spec.get("absent", []):
        if tok in tokens:
            failures.append(f"{where}: SSE event {tok!r} must be absent")
    if "counts" in spec:
        from collections import Counter as _Counter
        got = _Counter(tokens)
        for tok, want in spec["counts"].items():
            if got.get(tok, 0) != want:
                failures.append(
                    f"{where}: SSE event {tok!r} count {got.get(tok, 0)}, want {want}")
    if spec.get("sequence_numbers"):
        payloads, errors = _json_frames(frames)
        failures += [f"{where}: {e}" for e in errors]
        for i, p in enumerate(payloads):
            if not isinstance(p, dict) or p.get("sequence_number") != i:
                failures.append(
                    f"{where}: frame {i} sequence_number "
                    f"{p.get('sequence_number') if isinstance(p, dict) else p!r}, want {i}")
                break
    if spec.get("done") is True and "[DONE]" not in tokens:
        failures.append(f"{where}: SSE stream missing data: [DONE]")
    if spec.get("done") is False and "[DONE]" in tokens:
        failures.append(f"{where}: SSE stream must not contain data: [DONE]")
    integrity = spec.get("integrity")
    if integrity == "responses":
        failures += check_responses_integrity(frames, where)
    elif integrity == "chat":
        failures += check_chat_integrity(frames, where, spec.get("joined_tool_args"))
    elif integrity == "anthropic":
        failures += check_anthropic_integrity(frames, where, spec.get("joined_tool_args"))
    elif integrity == "gemini":
        failures += check_gemini_integrity(frames, where, spec.get("joined_args"),
                                           spec.get("joined_text"))
    elif integrity is not None:
        failures.append(f"{where}: unknown sse.integrity {integrity!r}")
    return failures


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class FakeUpstream:
    """Records requests; serves one scripted response per case."""

    def __init__(self, script: dict):
        self.script = script
        self.requests: list[dict] = []
        self._server = http.server.ThreadingHTTPServer(
            ("127.0.0.1", 0), self._handler())
        self.port = self._server.server_address[1]
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.05},
            daemon=True)
        self._thread.start()

    def _handler(self):
        parent = self

        class H(http.server.BaseHTTPRequestHandler):
            def _serve(self):
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length) if length else b""
                raw_path = self.path
                path, _, query = raw_path.partition("?")
                parent.requests.append({
                    "method": self.command,
                    "path": path,
                    "query": query,
                    "full_path": raw_path,
                    "headers": {k.lower(): v for k, v in self.headers.items()},
                    "body": body,
                })
                seq = parent.script.get("sequence")
                if seq:
                    idx = min(len(parent.requests) - 1, len(seq) - 1)
                    script = seq[idx]
                else:
                    script = parent.script
                if "redirect" in script:
                    self.send_response(script.get("redirect_status", 302))
                    self.send_header("Location", script["redirect"])
                    self.end_headers()
                    return
                if "sse_frames" in script:
                    payload = "".join(script["sse_frames"]).encode()
                    if script.get("truncate_after") is not None:
                        payload = payload[:script["truncate_after"]]
                    self.send_response(script.get("status", 200))
                    self.send_header("Content-Type", "text/event-stream")
                    self.end_headers()
                    try:
                        self.wfile.write(payload)
                        self.wfile.flush()
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    return
                raw = script.get("raw_body", "")
                if isinstance(raw, dict):
                    raw = json.dumps(raw)
                data = raw.encode() if isinstance(raw, str) else raw
                if "gzip_json" in script:
                    import gzip as _gzip
                    data = _gzip.compress(json.dumps(script["gzip_json"]).encode())
                    self.send_response(script.get("status", 200))
                    seen_ct = False
                    for k, v in script.get("headers", {}).items():
                        self.send_header(k, v)
                        if k.lower() == "content-type":
                            seen_ct = True
                    if not seen_ct:
                        self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Encoding", "gzip")
                    self.end_headers()
                    try:
                        self.wfile.write(data)
                        self.wfile.flush()
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    return
                self.send_response(script.get("status", 200))
                for k, v in script.get("headers", {"Content-Type": "application/json"}).items():
                    self.send_header(k, v)
                self.end_headers()
                try:
                    self.wfile.write(data)
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    pass

            do_POST = _serve
            do_GET = _serve

            def log_message(self, *args):
                pass

        return H

    def close(self):
        self._server.shutdown()
        self._server.server_close()


def wait_ready(url: str, timeout: float = 25.0) -> None:
    end = time.time() + timeout
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, timeout=2) as r:
                if r.status == 200:
                    return
        except Exception:
            time.sleep(0.2)
    raise RuntimeError(f"backend not ready: {url}")


def hermetic_env(tmp: str) -> dict:
    """Subprocess environment that cannot read the user's live credentials.

    Points HOME and XDG_CONFIG_HOME at fresh per-case directories so neither
    backend auto-loads ~/.config/ai/credentials.env (or the XDG equivalent)
    via config.Load. Only the orchestrating shell's environment is copied;
    the process-wide HOME/XDG_CONFIG_HOME are never overwritten.
    """
    home = os.path.join(tmp, "home")
    xdg = os.path.join(tmp, "xdg")
    os.makedirs(home, exist_ok=True)
    os.makedirs(xdg, exist_ok=True)
    env = dict(os.environ)
    env["HOME"] = home
    env["XDG_CONFIG_HOME"] = xdg
    env.pop("MODEL_ROUTER_API_KEY", None)
    env.pop("MODEL_ROUTER_API_KEYS", None)
    env.pop("ROUTER_CONFIG", None)
    return env


def launch(backend: str, cfg_path: str, port: int, go_bin: str, log_path: str,
           tmp: str, python_bin: str | None = None) -> subprocess.Popen:
    if backend == "python":
        py = python_bin or default_python()
        env = hermetic_env(tmp)
        # Make the router package importable without a hardcoded
        # versioned site-packages path.
        env["PYTHONPATH"] = LEGACY_REPO + (":" + env["PYTHONPATH"] if env.get("PYTHONPATH") else "")
        cmd = [py, "-m", "model_router", "-c", cfg_path,
               "--host", "127.0.0.1", "--port", str(port), "--no-default-env"]
    else:
        env = hermetic_env(tmp)
        cmd = [go_bin, "--config", cfg_path, "--host", "127.0.0.1",
               "--port", str(port)]
    log = open(log_path, "ab")
    return subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT,
                            cwd=LEGACY_REPO if backend == "python" else REPO, env=env)


def resolve_path(obj, path: str):
    """Dot-path lookup: 'choices.0.finish_reason'. Raises KeyError."""
    node = obj
    for part in path.split("."):
        if isinstance(node, list):
            node = node[int(part)]
        else:
            node = node[part]
    return node


def check_json(actual: dict | list, checks: dict, where: str, failures: list) -> None:
    for path, want in checks.items():
        try:
            got = resolve_path(actual, path)
        except (KeyError, IndexError, ValueError, TypeError):
            failures.append(f"{where}: missing path {path}")
            continue
        if isinstance(want, dict) and "__contains__" in want:
            if want["__contains__"] not in json.dumps(got):
                failures.append(f"{where}: {path} lacks {want['__contains__']!r} (got {got!r})")
        elif got != want:
            failures.append(f"{where}: {path} = {got!r}, want {want!r}")


def run_case(backend: str, case: dict, go_bin: str,
             python_bin: str | None = None) -> list[str]:
    failures: list[str] = []
    upstream = FakeUpstream(case["upstream"])
    port = free_port()
    import tempfile as _tf
    with _tf.TemporaryDirectory(prefix="conform-") as tmp:
        cfg_text = case["config"].replace("{UPSTREAM}", f"http://127.0.0.1:{upstream.port}")
        cfg_text = cfg_text.replace("{PORT}", str(port))
        cfg_path = os.path.join(tmp, f"conform-{case['name']}.toml")
        with open(cfg_path, "w") as f:
            f.write(cfg_text)
        proc = launch(backend, cfg_path, port, go_bin, os.path.join(tmp, f"conform-{backend}.log"), tmp,
                      python_bin)
        try:
            wait_ready(f"http://127.0.0.1:{port}/livez")
            req = case["request"]
            data = req.get("body")
            if isinstance(data, dict):
                data = json.dumps(data, separators=(",", ":")).encode()
            elif isinstance(data, str):
                data = data.encode()
            http_req = urllib.request.Request(
                f"http://127.0.0.1:{port}{req['path']}",
                data=data, method=req.get("method", "POST"),
                headers={"Content-Type": "application/json",
                         **req.get("headers", {})})
            try:
                with urllib.request.urlopen(http_req, timeout=30) as r:
                    status, headers, raw = r.status, dict(r.headers), r.read()
            except urllib.error.HTTPError as e:
                status, headers, raw = e.code, dict(e.headers), e.read()
            exp = case["expect"]
            if "status_in" in exp:
                if status not in exp["status_in"]:
                    failures.append(f"client status {status}, want one of {exp['status_in']} (body {raw[:300]!r})")
            elif status != exp["status"]:
                failures.append(f"client status {status}, want {exp['status']} (body {raw[:300]!r})")
            for name, want in exp.get("client_headers", {}).items():
                got = headers.get(name) or headers.get(name.lower())
                if isinstance(want, dict) and want.get("absent"):
                    if got is not None:
                        failures.append(f"client header {name} must be absent (got {got!r})")
                elif isinstance(want, dict) and want.get("present"):
                    if got is None:
                        failures.append(f"client header {name} must be present")
                elif got != want:
                    failures.append(f"client header {name} = {got!r}, want {want!r}")
            text = raw.decode("utf-8", "replace")
            if exp.get("has_done") and "data: [DONE]" not in text:
                failures.append("client stream missing data: [DONE]")
            if exp.get("no_done") and "data: [DONE]" in text:
                failures.append("client stream must not contain data: [DONE]")
            for needle in exp.get("client_text_contains", []):
                if needle not in text:
                    failures.append(f"client body lacks {needle!r} (got {text[:400]!r})")
            for needle in exp.get("client_text_absent", []):
                if needle in text:
                    failures.append(f"client body must not contain {needle!r}")
            if exp.get("sse") is not None:
                failures += check_sse_frames(parse_sse(text), exp["sse"], "client")
            if exp.get("gemini_array") is not None:
                spec = exp["gemini_array"]
                failures += check_gemini_array_body(
                    text, "client", spec.get("joined_args"),
                    spec.get("joined_text"))
            if "client_body" in exp:
                try:
                    actual = json.loads(text)
                except ValueError:
                    failures.append(f"client body not JSON: {text[:300]!r}")
                    actual = None
                if actual is not None:
                    check_json(actual, exp["client_body"].get("paths", {}), "client", failures)
                    for path in exp["client_body"].get("absent", []):
                        try:
                            resolve_path(actual, path)
                            failures.append(f"client body path {path} must be absent")
                        except (KeyError, IndexError, ValueError, TypeError):
                            pass
            uexp = exp.get("upstream")
            if uexp is not None:
                n_calls = len(upstream.requests)
                if "exact_requests" in uexp:
                    if n_calls != uexp["exact_requests"]:
                        failures.append(
                            f"upstream got {n_calls} requests, want exactly {uexp['exact_requests']}")
                if "max_requests" in uexp:
                    if n_calls > uexp["max_requests"]:
                        failures.append(
                            f"upstream got {n_calls} requests, want <= {uexp['max_requests']}")
                if n_calls == 0:
                    if uexp.get("exact_requests", 1) == 0 or uexp.get("max_requests", 1) == 0:
                        pass  # correctly never contacted the upstream
                    elif uexp.get("min_requests", 1) == 0:
                        pass  # legacy predispatch-refusal marker: see exact_requests
                    else:
                        failures.append("upstream received no requests")
                else:
                    seen = upstream.requests[uexp.get("index", -1)]
                    if seen["path"] != uexp["path"]:
                        failures.append(f"upstream path {seen['path']}, want {uexp['path']}")
                    if "query" in uexp:
                        if seen.get("query", "") != uexp["query"]:
                            failures.append(
                                f"upstream query {seen.get('query', '')!r}, "
                                f"want {uexp['query']!r}")
                    for absent in uexp.get("query_absent", []):
                        params = seen.get("query", "")
                        names = {kv.split("=", 1)[0] for kv in params.split("&")
                                 if kv}
                        if absent in names or f"{absent}=" in params:
                            failures.append(
                                f"upstream query must not forward {absent!r} "
                                f"(got {params!r})")
                    for name, want in uexp.get("headers", {}).items():
                        got = seen["headers"].get(name.lower())
                        if isinstance(want, dict) and want.get("absent"):
                            if got is not None:
                                failures.append(f"upstream header {name} must be absent (got {got!r})")
                        elif got != want:
                            failures.append(f"upstream header {name} = {got!r}, want {want!r}")
                    if uexp.get("raw_equal_request"):
                        if seen["body"] != data:
                            failures.append(f"upstream body not byte-identical:\n got {seen['body'][:300]!r}")
                    elif uexp.get("json_equal_request"):
                        try:
                            if json.loads(seen["body"] or b"null") != json.loads(data or b"null"):
                                failures.append("upstream body not semantically equal to request")
                        except ValueError:
                            failures.append(f"upstream body not JSON: {seen['body'][:300]!r}")
                    elif "body" in uexp:
                        try:
                            actual = json.loads(seen["body"] or b"null")
                        except ValueError:
                            failures.append(f"upstream body not JSON: {seen['body'][:300]!r}")
                            actual = None
                        if actual is not None:
                            check_json(actual, uexp["body"].get("paths", {}), "upstream", failures)
                            for path in uexp["body"].get("absent", []):
                                try:
                                    resolve_path(actual, path)
                                    failures.append(f"upstream body path {path} must be absent")
                                except (KeyError, IndexError, ValueError, TypeError):
                                    pass
                _min = uexp.get("min_requests", 0 if ("exact_requests" in uexp or "max_requests" in uexp) else 1)
                if _min > len(upstream.requests):
                    failures.append(f"upstream got {len(upstream.requests)} requests, want >= {_min}")
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
            upstream.close()
    return failures


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--backend", choices=["python", "go"], required=True)
    ap.add_argument("--go-bin", default=os.path.join(REPO, "model-router-go"))
    ap.add_argument("--python-bin", default=None,
                    help="Python-router interpreter (default: $MODEL_ROUTER_PYTHON, "
                         "legacy checkout .venv, or this interpreter)")
    ap.add_argument("--case", default=None)
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args()
    names = sorted(f[:-5] for f in os.listdir(CASES) if f.endswith(".json"))
    if args.list:
        print("\n".join(names))
        return 0
    if args.case:
        names = [args.case]
    total_fail = 0
    for name in names:
        with open(os.path.join(CASES, name + ".json")) as f:
            case = json.load(f)
        try:
            failures = run_case(args.backend, case, args.go_bin, args.python_bin)
        except Exception as e:  # keep other cases running
            failures = [f"harness error: {e}"]
        if failures:
            total_fail += 1
            print(f"FAIL {name}")
            for fl in failures:
                print(f"  - {fl}")
        else:
            print(f"ok {name}")
    print(f"{len(names) - total_fail}/{len(names)} passed [{args.backend}]")
    return 1 if total_fail else 0


if __name__ == "__main__":
    sys.exit(main())
