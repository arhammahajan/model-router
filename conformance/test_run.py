#!/usr/bin/env python3
"""Unit tests for the conformance runner's SSE assertions (stdlib only).

Covers the parser properties the external cases depend on: framing is
recognized only at line starts (never inside JSON text), fragmented
multi-line framing is handled, and the identity/sequence relationships
reject duplication, reordering and substitution. These tests assert
checker behavior, not the shape of any single helper body.

Run: python3 -m unittest test_run -v   (from go/conformance/)
"""
from __future__ import annotations

import json
import os
import unittest

import run as runner


def frame(event, payload):
    body = payload if isinstance(payload, str) else json.dumps(payload)
    head = f"event: {event}\n" if event is not None else ""
    return f"{head}data: {body}\n\n"


def responses_text_stream(text="hello", item="msg_1", call=None):
    """Canonical Responses text lifecycle with strict 0..n sequence numbers."""
    base = {"id": "resp_1", "object": "response", "status": "in_progress",
            "model": "m", "output": []}
    events = [
        ("response.created", {"type": "response.created", "response": base}),
        ("response.in_progress", {"type": "response.in_progress", "response": base}),
        ("response.output_item.added",
         {"type": "response.output_item.added", "output_index": 0,
          "item": {"id": item, "type": "message", "status": "in_progress"}}),
        ("response.content_part.added",
         {"type": "response.content_part.added", "item_id": item, "output_index": 0,
          "content_index": 0, "part": {"type": "output_text", "text": ""}}),
        ("response.output_text.delta",
         {"type": "response.output_text.delta", "item_id": item, "output_index": 0,
          "content_index": 0, "delta": text}),
        ("response.output_text.done",
         {"type": "response.output_text.done", "item_id": item, "output_index": 0,
          "content_index": 0, "text": text}),
        ("response.content_part.done",
         {"type": "response.content_part.done", "item_id": item, "output_index": 0,
          "content_index": 0, "part": {"type": "output_text", "text": text}}),
        ("response.output_item.done",
         {"type": "response.output_item.done", "output_index": 0,
          "item": {"id": item, "type": "message", "status": "completed",
                   "content": [{"type": "output_text", "text": text}]}}),
        ("response.completed",
         {"type": "response.completed",
          "response": {**base, "status": "completed",
                       "output": [{"id": item, "type": "message", "status": "completed",
                                   "content": [{"type": "output_text", "text": text}]}]}}),
    ]
    out = []
    for i, (name, payload) in enumerate(events):
        out.append(frame(name, {**payload, "sequence_number": i}))
    return "".join(out)


def chat_tool_stream(args='{"n":9007199254740993}', chunk_id="chatcmpl-7"):
    base = {"id": chunk_id, "object": "chat.completion.chunk", "created": 1, "model": "m"}
    half = len(args) // 2
    chunks = [
        {**base, "choices": [{"index": 0,
                              "delta": {"role": "assistant"}, "finish_reason": None}]},
        {**base, "choices": [{"index": 0, "delta": {"tool_calls": [
            {"index": 0, "id": "call_1", "type": "function",
             "function": {"name": "get_time", "arguments": args[:half]}}]},
                              "finish_reason": None}]},
        {**base, "choices": [{"index": 0, "delta": {"tool_calls": [
            {"index": 0, "function": {"arguments": args[half:]}}]},
                              "finish_reason": None}]},
        {**base, "choices": [{"index": 0, "delta": {},
                              "finish_reason": "tool_calls"}]},
    ]
    return "".join(frame(None, c) for c in chunks) + "data: [DONE]\n\n"


def anthropic_text_stream(text="hello"):
    start = {"type": "message_start",
             "message": {"id": "msg_1", "type": "message", "role": "assistant",
                         "model": "m", "content": [], "stop_reason": None,
                         "usage": {"input_tokens": 0}}}
    blocks = [
        ("content_block_start",
         {"type": "content_block_start", "index": 0,
          "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta",
         {"type": "content_block_delta", "index": 0,
          "delta": {"type": "text_delta", "text": text}}),
        ("content_block_stop",
         {"type": "content_block_stop", "index": 0}),
    ]
    tail = [
        ("message_delta",
         {"type": "message_delta",
          "delta": {"stop_reason": "end_turn"},
          "usage": {"output_tokens": 3}}),
        ("message_stop", {"type": "message_stop"}),
    ]
    events = [("message_start", start)] + blocks + tail
    return "".join(frame(name, payload) for name, payload in events)


def anthropic_tool_stream(partials=('{"a":1}',), seed_input=None,
                          stop="tool_use", out_tokens=7):
    """Canonical Anthropic tool_use lifecycle with fragmented input JSON."""
    start = {"type": "message_start",
             "message": {"id": "msg_1", "type": "message", "role": "assistant",
                         "model": "m", "content": [], "stop_reason": None,
                         "usage": {"input_tokens": 0}}}
    events = [("message_start", start)]
    block = {"type": "tool_use", "id": "call_1", "name": "get_time",
             "input": {} if seed_input is None else seed_input}
    events.append(("content_block_start",
                   {"type": "content_block_start", "index": 0,
                    "content_block": block}))
    for frag in partials:
        events.append(("content_block_delta",
                       {"type": "content_block_delta", "index": 0,
                        "delta": {"type": "input_json_delta",
                                  "partial_json": frag}}))
    events.append(("content_block_stop",
                   {"type": "content_block_stop", "index": 0}))
    events.append(("message_delta",
                   {"type": "message_delta",
                    "delta": {"stop_reason": stop},
                    "usage": {"output_tokens": out_tokens}}))
    events.append(("message_stop", {"type": "message_stop"}))
    return "".join(frame(name, payload) for name, payload in events)


def gemini_text_stream(text="hello"):
    """Official-shaped Gemini SSE stream: text deltas plus terminal usage."""
    half = len(text) // 2
    chunks = [
        {"candidates": [{"content": {"role": "model",
                                     "parts": [{"text": text[:half]}]},
                          "index": 0}],
         "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
         "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 1,
                           "totalTokenCount": 8}},
        {"candidates": [{"content": {"role": "model",
                                     "parts": [{"text": text[half:]}]},
                          "index": 0, "finishReason": "STOP"}],
         "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
         "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 3,
                           "totalTokenCount": 10}},
    ]
    return "".join(frame(None, c) for c in chunks)


def gemini_tool_stream():
    """Official-shaped Gemini tool stream: exact bigint args, empty args."""
    first = {"candidates": [{"content": {"role": "model",
                                         "parts": [{"text": "a"}]},
                              "index": 0}],
             "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
             "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 1,
                               "totalTokenCount": 8}}
    terminal = {
        "candidates": [{"content": {
            "role": "model",
            "parts": [
                {"functionCall": {"name": "get_time", "id": "call_1",
                                  "args": {"tz": "UTC",
                                           "n": 9007199254740993}}},
                {"functionCall": {"name": "get_weather", "id": "call_2",
                                  "args": {}}},
            ]}, "index": 0, "finishReason": "STOP"}],
        "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
        "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 3,
                          "totalTokenCount": 10}}
    return frame(None, first) + frame(None, terminal)


def gemini_bad_tool_stream(args_json, name="f"):
    """Terminal Gemini chunk whose functionCall carries invalid args/name."""
    first = {"candidates": [{"content": {"role": "model",
                                         "parts": [{"text": "a"}]},
                              "index": 0}],
             "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
             "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 1,
                               "totalTokenCount": 8}}
    bad_args = json.loads(args_json)  # scalar/array stays non-object
    terminal = {
        "candidates": [{"content": {
            "role": "model",
            "parts": [{"functionCall": {"name": name,
                                        "args": bad_args}}]},
            "index": 0, "finishReason": "STOP"}],
        "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
        "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 3,
                          "totalTokenCount": 10}}
    return frame(None, first) + frame(None, terminal)


def gemini_blocked_stream(finish):
    """Blocked-prompt signal: no candidates, terminal filter accounting."""
    chunk = {"promptFeedback": {"blockReason": "SAFETY"},
             "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
             "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 0,
                               "totalTokenCount": 7}}
    if finish != "":
        chunk["candidates"] = [{"content": {"role": "model", "parts": []},
                                "index": 0, "finishReason": finish}]
    return frame(None, chunk)


def gemini_array_body():
    """Official-shaped Gemini JSON-array body: text plus one tool object."""
    first = {"candidates": [{"content": {"role": "model",
                                         "parts": [{"text": "a"}]},
                              "index": 0}],
             "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
             "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 1,
                               "totalTokenCount": 8}}
    terminal = {
        "candidates": [{"content": {
            "role": "model",
            "parts": [
                {"text": "b"},
                {"functionCall": {"name": "get_time", "id": "call_1",
                                  "args": {"tz": "UTC",
                                           "n": 9007199254740993}}},
            ]}, "index": 0, "finishReason": "STOP"}],
        "modelVersion": "gemini-2.0-flash", "responseId": "resp_gem_1",
        "usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 3,
                          "totalTokenCount": 10}}
    return "[" + json.dumps(first) + "," + json.dumps(terminal) + "]"


def _drop_event(body, event, occurrence=1):
    """Remove the nth frame with the given SSE event name."""
    kept = []
    seen = 0
    for f in runner.parse_sse(body):
        if f["event"] == event and seen < occurrence:
            seen += 1
            if seen == occurrence:
                continue
        kept.append(f)
    out = []
    for f in kept:
        head = f"event: {f['event']}\n" if f["event"] is not None else ""
        out.append(f"{head}data: {f['data']}\n\n")
    return "".join(out)


def _swap_event(body, old, new, occurrence=1):
    """Rename the nth frame's SSE event label, keeping its payload."""
    kept = []
    seen = 0
    for f in runner.parse_sse(body):
        if f["event"] == old and seen < occurrence:
            seen += 1
            if seen == occurrence:
                f = {"event": new, "data": f["data"]}
        kept.append(f)
    out = []
    for f in kept:
        head = f"event: {f['event']}\n" if f["event"] is not None else ""
        out.append(f"{head}data: {f['data']}\n\n")
    return "".join(out)


class ParseTest(unittest.TestCase):
    def test_event_token_inside_json_text_is_not_framing(self):
        body = ('data: {"type":"output_text","text":"say event: error loudly"}\n\n'
                'data: {"type":"done"}\n\n')
        frames = runner.parse_sse(body)
        self.assertEqual(len(frames), 2)
        self.assertIsNone(frames[0]["event"])
        self.assertIn("event: error", frames[0]["data"])

    def test_colon_comment_and_crlf_and_multi_data(self):
        body = (": upstream comment\r\n"
                "event: response.created\r\n"
                "data: {\"a\":1}\r\n"
                "data: {\"b\":2}\r\n\r\n")
        frames = runner.parse_sse(body)
        self.assertEqual(frames, [{"event": "response.created",
                                   "data": '{"a":1}\n{"b":2}'}])

    def test_single_space_strip_only(self):
        frames = runner.parse_sse("data:  spaced\n\n")
        self.assertEqual(frames[0]["data"], " spaced")

    def test_stray_lines_do_not_create_frames(self):
        frames = runner.parse_sse("garbage line\n\n")
        self.assertEqual(frames, [])

    def test_fragmented_deltas_stay_separate_frames(self):
        body = ('event: response.function_call_arguments.delta\n'
                'data: {"delta":"{\\"n\\":"}\n\n'
                'event: response.function_call_arguments.delta\n'
                'data: {"delta":"9007199254740993}"}\n\n')
        frames = runner.parse_sse(body)
        self.assertEqual([f["event"] for f in frames],
                         ["response.function_call_arguments.delta"] * 2)
        self.assertIn("9007199254740993", frames[1]["data"])


class SequenceTest(unittest.TestCase):
    def test_strict_sequence_accepts_canonical_stream(self):
        frames = runner.parse_sse(responses_text_stream())
        self.assertEqual(runner.check_sse_frames(
            frames, {"sequence_numbers": True}, "client"), [])

    def test_sequence_gap_rejected(self):
        body = responses_text_stream().replace('"sequence_number": 4',
                                               '"sequence_number": 9')
        frames = runner.parse_sse(body)
        failures = runner.check_sse_frames(frames, {"sequence_numbers": True}, "client")
        self.assertTrue(any("sequence_number" in f for f in failures))

    def test_sequence_missing_rejected(self):
        body = responses_text_stream().replace(', "sequence_number": 0}', '}', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_sse_frames(frames, {"sequence_numbers": True}, "client")
        self.assertTrue(any("sequence_number" in f for f in failures))


class ResponsesIntegrityTest(unittest.TestCase):
    def test_canonical_stream_passes(self):
        frames = runner.parse_sse(responses_text_stream())
        self.assertEqual(runner.check_responses_integrity(frames, "client"), [])

    def test_delta_for_unknown_item_rejected(self):
        body = responses_text_stream().replace(
            '"type": "response.output_text.delta", "item_id": "msg_1"',
            '"type": "response.output_text.delta", "item_id": "msg_X"', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_responses_integrity(frames, "client")
        self.assertTrue(any("unknown item" in f for f in failures))

    def test_terminal_duplicating_deltas_rejected(self):
        body = responses_text_stream().replace(
            '"text": "hello", "sequence_number": 5}',
            '"text": "hellohello", "sequence_number": 5}', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_responses_integrity(frames, "client")
        # The doubled done-text no longer equals the single joined delta.
        self.assertTrue(failures)

    def test_terminal_changing_identity_rejected(self):
        body = responses_text_stream().replace('"id": "msg_1", "type": "message", "status": "completed"',
                                               '"id": "msg_2", "type": "message", "status": "completed"')
        frames = runner.parse_sse(body)
        failures = runner.check_responses_integrity(frames, "client")
        self.assertTrue(any("completed" in f for f in failures))

    def test_response_id_must_stay_stable(self):
        body = responses_text_stream().replace('"sequence_number": 8}',
                                               '"sequence_number": 8, "response": {"id": "resp_other"}}', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_responses_integrity(frames, "client")
        self.assertTrue(any("response id changes" in f for f in failures))

    def test_missing_terminal_rejected(self):
        frames = runner.parse_sse(responses_text_stream())
        truncated = [f for f in frames if f["event"] != "response.completed"]
        failures = runner.check_responses_integrity(truncated, "client")
        self.assertTrue(any("response.completed" in f for f in failures))


class ChatIntegrityTest(unittest.TestCase):
    def test_fragmented_tool_args_pass(self):
        frames = runner.parse_sse(chat_tool_stream())
        self.assertEqual(runner.check_chat_integrity(frames, "client"), [])

    def test_changing_chunk_id_rejected(self):
        body = chat_tool_stream().replace("chatcmpl-7", "chatcmpl-8", 1)
        frames = runner.parse_sse(body)
        failures = runner.check_chat_integrity(frames, "client")
        self.assertTrue(any("stable" in f for f in failures))

    def test_missing_done_rejected(self):
        frames = runner.parse_sse(chat_tool_stream())
        failures = runner.check_chat_integrity(frames[:-1], "client")
        self.assertTrue(any("[DONE]" in f for f in failures))

    def test_changing_tool_id_rejected(self):
        body = chat_tool_stream().replace('{"index": 0, "function": {',
                                          '{"index": 0, "id": "call_2", "function": {', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_chat_integrity(frames, "client")
        self.assertTrue(any("tool_call id changed" in f for f in failures))

    def test_truncated_stream_has_no_terminal(self):
        frames = runner.parse_sse(chat_tool_stream()[:200])
        tokens = [runner.sse_token(f) for f in frames]
        self.assertNotIn("[DONE]", tokens)

    def test_joined_tool_args_reassembled(self):
        frames = runner.parse_sse(chat_tool_stream())
        ok = runner.check_chat_integrity(
            frames, "client",
            {"0": '{"n":9007199254740993}'})
        self.assertEqual(ok, [])
        bad = runner.check_chat_integrity(
            frames, "client",
            {"0": '{"n":1}'})
        self.assertTrue(any("reassemble" in f for f in bad))


class SpecTest(unittest.TestCase):
    def test_exact_event_order_and_counts(self):
        frames = runner.parse_sse(responses_text_stream())
        tokens = [runner.sse_token(f) for f in frames]
        ok = runner.check_sse_frames(frames, {"events": tokens,
                                              "counts": {"response.created": 1,
                                                         "response.completed": 1}},
                                     "client")
        self.assertEqual(ok, [])
        bad = runner.check_sse_frames(frames, {"events": list(reversed(tokens))}, "client")
        self.assertTrue(bad)
        missing = runner.check_sse_frames(frames, {"absent": ["response.completed"]},
                                          "client")
        self.assertTrue(any("must be absent" in f for f in missing))

    def test_done_flag(self):
        frames = runner.parse_sse(chat_tool_stream())
        self.assertEqual(runner.check_sse_frames(frames, {"done": True}, "client"), [])
        self.assertTrue(runner.check_sse_frames(frames, {"done": False}, "client"))
        partial = runner.parse_sse(chat_tool_stream()[:200])
        self.assertEqual(runner.check_sse_frames(partial, {"done": False}, "client"), [])

    def test_unknown_integrity_rejected(self):
        frames = runner.parse_sse(chat_tool_stream())
        failures = runner.check_sse_frames(frames, {"integrity": "bogus"}, "client")
        self.assertTrue(any("unknown sse.integrity" in f for f in failures))


class AnthropicIntegrityTest(unittest.TestCase):
    def test_canonical_stream_passes(self):
        frames = runner.parse_sse(anthropic_text_stream())
        self.assertEqual(runner.check_anthropic_integrity(frames, "client"), [])

    def test_delta_for_unknown_index_rejected(self):
        body = anthropic_text_stream().replace(
            '"type": "content_block_delta", "index": 0',
            '"type": "content_block_delta", "index": 7', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("unknown block index" in f for f in failures))

    def test_missing_stop_rejected(self):
        frames = runner.parse_sse(anthropic_text_stream())
        truncated = [f for f in frames if f["event"] != "message_stop"]
        failures = runner.check_anthropic_integrity(truncated, "client")
        self.assertTrue(any("message_stop" in f for f in failures))

    def test_done_token_rejected(self):
        frames = runner.parse_sse(anthropic_text_stream() + "data: [DONE]\n\n")
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("[DONE]" in f for f in failures))

    def test_event_token_inside_json_text_is_not_framing(self):
        body = anthropic_text_stream("say event: message_stop loudly")
        frames = runner.parse_sse(body)
        self.assertEqual(runner.check_anthropic_integrity(frames, "client"), [])

    def test_missing_terminal_delta_rejected(self):
        # Root repro: dropping message_delta must fail (terminal usage is
        # authoritative and required), not return [].
        body = _drop_event(anthropic_text_stream(), "message_delta")
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("message_delta" in f for f in failures))

    def test_nonnull_unmapped_usage_rejected(self):
        for key, value in (("server_tool_use", {"web_search_requests": 1}),
                           ("service_tier", "priority"),
                           ("output_tokens_details", {"reasoning_tokens": 1})):
            with self.subTest(field=key):
                frames = runner.parse_sse(anthropic_text_stream())
                payload = json.loads(frames[0]["data"])
                payload["message"]["usage"][key] = value
                frames[0]["data"] = json.dumps(payload)
                self.assertTrue(runner.check_anthropic_integrity(frames, "client"))

    def test_terminal_delta_without_usage_rejected(self):
        body = anthropic_text_stream().replace(
            '"delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 3}',
            '"delta": {"stop_reason": "end_turn"}', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("usage" in f for f in failures))

    def test_duplicate_start_rejected(self):
        frames = runner.parse_sse(anthropic_text_stream())
        doubled = frames[:1] + frames
        failures = runner.check_anthropic_integrity(doubled, "client")
        self.assertTrue(any("message_start" in f for f in failures))

    def test_duplicate_stop_rejected(self):
        frames = runner.parse_sse(anthropic_text_stream())
        doubled = frames + frames[-1:]
        failures = runner.check_anthropic_integrity(doubled, "client")
        self.assertTrue(any("message_stop" in f for f in failures))

    def test_event_type_mismatch_rejected(self):
        body = _swap_event(anthropic_text_stream(), "message_delta", "ping")
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("mismatch" in f for f in failures))

    def test_negative_indices_rejected(self):
        # The whole block lifecycle moves to index -1: the old checker
        # accepts it because it only tests index type, never sign.
        body = anthropic_text_stream().replace('"index": 0', '"index": -1')
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("index" in f for f in failures))

    def test_duplicate_block_start_rejected(self):
        frames = runner.parse_sse(anthropic_text_stream())
        doubled = frames[:2] + frames[1:]
        failures = runner.check_anthropic_integrity(doubled, "client")
        self.assertTrue(any("duplicate" in f for f in failures))

    def test_delta_after_stop_rejected(self):
        frames = runner.parse_sse(anthropic_tool_stream())
        order = [frames[0], frames[1]] + [frames[3], frames[2]] + frames[4:]
        failures = runner.check_anthropic_integrity(order, "client")
        self.assertTrue(any("stop" in f for f in failures))

    def test_content_after_terminal_delta_rejected(self):
        # A block stop moved past message_delta: framing still opens with
        # message_start and closes with message_stop, so only an explicit
        # phase check can catch it.
        frames = runner.parse_sse(anthropic_text_stream())
        start, cstart, cdelta, cstop, mdelta, mstop = frames
        order = [start, cstart, cdelta, mdelta, cstop, mstop]
        failures = runner.check_anthropic_integrity(order, "client")
        self.assertTrue(any("message_delta" in f for f in failures))

    def test_reused_tool_id_across_blocks_rejected(self):
        second_start = frame("content_block_start",
                            {"type": "content_block_start", "index": 1,
                             "content_block": {"type": "tool_use", "id": "call_1",
                                               "name": "other", "input": {}}})
        second_stop = frame("content_block_stop",
                            {"type": "content_block_stop", "index": 1})
        frames = runner.parse_sse(anthropic_tool_stream())
        injected = (frames[:-2] + runner.parse_sse(second_start + second_stop)
                    + frames[-2:])
        failures = runner.check_anthropic_integrity(injected, "client")
        self.assertTrue(any("tool_use" in f for f in failures))

    def test_tool_block_without_identity_rejected(self):
        body = anthropic_tool_stream().replace(
            '"content_block": {"type": "tool_use", "id": "call_1", '
            '"name": "get_time", "input": {}}',
            '"content_block": {"type": "tool_use", "input": {}}', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("tool_use" in f for f in failures))

    def test_text_delta_on_tool_block_rejected(self):
        body = anthropic_tool_stream().replace(
            '"type": "input_json_delta", "partial_json": "{\\"a\\":1}"',
            '"type": "text_delta", "text": "hi"', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("delta" in f for f in failures))

    def test_input_json_delta_on_text_block_rejected(self):
        body = anthropic_text_stream().replace(
            '"type": "text_delta", "text": "hello"',
            '"type": "input_json_delta", "partial_json": "{\\"a\\":1}"', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("delta" in f for f in failures))

    def test_malformed_tool_args_rejected(self):
        frames = runner.parse_sse(anthropic_tool_stream(('{"a":',)))
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("args" in f for f in failures))

    def test_non_object_tool_args_rejected(self):
        frames = runner.parse_sse(anthropic_tool_stream(('[1,2]',)))
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("args" in f for f in failures))

    def test_split_large_integer_args_reassemble_exactly(self):
        frags = ('{"tz":"UTC","n":90071992547', '40993}',)
        frames = runner.parse_sse(anthropic_tool_stream(frags))
        self.assertEqual(runner.check_anthropic_integrity(frames, "client"), [])
        joined = "".join(frags)
        self.assertEqual(json.loads(joined)["n"], 9007199254740993)

    def test_expected_tool_arguments_ignore_chunk_boundaries(self):
        frames = runner.parse_sse(anthropic_tool_stream(
            ('{"n":90071992547', '40993}')))
        spec = {"integrity": "anthropic",
                "joined_tool_args": {"0": '{"n":9007199254740993}'}}
        self.assertEqual(runner.check_sse_frames(frames, spec, "client"), [])
        spec["joined_tool_args"]["0"] = '{"n":9007199254740992}'
        self.assertTrue(runner.check_sse_frames(frames, spec, "client"))

    def test_unmapped_stop_reason_rejected(self):
        body = anthropic_text_stream().replace('"stop_reason": "end_turn"',
                                               '"stop_reason": "pause_turn"', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("stop_reason" in f for f in failures))

    def test_unexpected_top_level_field_rejected(self):
        body = anthropic_text_stream().replace(
            '"type": "message_delta",',
            '"type": "message_delta", "session": 1,', 1)
        frames = runner.parse_sse(body)
        failures = runner.check_anthropic_integrity(frames, "client")
        self.assertTrue(any("unexpected" in f for f in failures))

    def test_start_shape_rejected(self):
        for label, good, bad in (
                ("role", '"role": "assistant"', '"role": "user"'),
                ("content", '"content": []',
                 '"content": [{"type": "text"}]'),
                ("usage", '"input_tokens": 0', '"input_tokens": -2')):
            body = anthropic_text_stream().replace(good, bad, 1)
            frames = runner.parse_sse(body)
            failures = runner.check_anthropic_integrity(frames, "client")
            self.assertTrue(failures, f"bad start accepted: {label}")

    def test_empty_text_block_without_deltas_passes(self):
        body = _drop_event(anthropic_text_stream(""), "content_block_delta")
        frames = runner.parse_sse(body)
        self.assertEqual(runner.check_anthropic_integrity(frames, "client"), [])

    def test_seeded_text_block_without_deltas_passes(self):
        seeded = frame("content_block_start",
                       {"type": "content_block_start", "index": 0,
                        "content_block": {"type": "text", "text": "seeded"}})
        frames = runner.parse_sse(anthropic_text_stream("seeded"))
        rebuilt = [frames[0]] + runner.parse_sse(seeded) + [
            f for f in frames[1:]
            if f["event"] not in ("content_block_start", "content_block_delta")]
        self.assertEqual(runner.check_anthropic_integrity(rebuilt, "client"), [])

    def test_seeded_tool_input_without_deltas_passes(self):
        frames = runner.parse_sse(
            anthropic_tool_stream((), {"n": 9007199254740993}))
        self.assertEqual(runner.check_anthropic_integrity(frames, "client"), [])

    def test_ping_frames_allowed(self):
        ping = frame("ping", {"type": "ping"})
        body = ping + anthropic_text_stream() + ping
        frames = runner.parse_sse(body)
        self.assertEqual(runner.check_anthropic_integrity(frames, "client"), [])


class GeminiIntegrityTest(unittest.TestCase):

    def test_chat_assistant_role_is_not_gemini_model_role(self):
        frames = runner.parse_sse(gemini_text_stream())
        payload = json.loads(frames[0]["data"])
        payload["candidates"][0]["content"]["role"] = "assistant"
        frames[0]["data"] = json.dumps(payload)
        self.assertTrue(runner.check_gemini_integrity(frames, "client"))
    """Independent Gemini stream validator (GenerateContentResponse chunks).

    Primary sources (no Vertex-only claims):
    - Gemini REST GenerateContentResponse/Candidate/usageMetadata/Part/
      FunctionCall: https://ai.google.dev/api/generate-content
    - Chat completions response/streaming (finish reasons, tool calls,
      terminal usage, [DONE]):
      https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create

    The validator checks downstream Gemini shapes only: data-only SSE
    frames or decoded JSON-array elements with candidate index 0 / role
    model, stable modelVersion/responseId when present, text plus complete
    functionCall parts with object args and unique call IDs/names in order,
    exactly one terminal finishReason on the last frame, authoritative
    terminal usageMetadata with total == prompt+candidates+thoughts+tool-use,
    and no [DONE] sentinel. It reuses the shared parse_sse helpers and
    never retains a full generation in production; these finite fixtures
    exercise the same checks the external runner applies.
    """

    def test_canonical_text_stream_passes(self):
        frames = runner.parse_sse(gemini_text_stream())
        self.assertEqual(runner.check_gemini_integrity(frames, "client"), [])

    def test_canonical_stream_rejects_event_names(self):
        body = ("event: message_start\n"
                'data: {"candidates":[]}\n\n')
        frames = runner.parse_sse(body)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("data-only" in f for f in failures))

    def test_done_sentinel_rejected(self):
        frames = runner.parse_sse(gemini_text_stream() + "data: [DONE]\n\n")
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("[DONE]" in f for f in failures))

    def test_event_token_inside_text_is_not_framing(self):
        body = gemini_text_stream("say event: error loudly")
        frames = runner.parse_sse(body)
        self.assertEqual(runner.check_gemini_integrity(frames, "client"), [])

    def test_missing_terminal_usage_rejected(self):
        frames = runner.parse_sse(gemini_text_stream())
        payload = json.loads(frames[-1]["data"])
        del payload["usageMetadata"]
        frames[-1]["data"] = json.dumps(payload)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("usageMetadata" in f for f in failures))

    def test_usage_total_mismatch_rejected(self):
        frames = runner.parse_sse(gemini_text_stream())
        payload = json.loads(frames[-1]["data"])
        payload["usageMetadata"]["totalTokenCount"] = 999
        frames[-1]["data"] = json.dumps(payload)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("total" in f for f in failures))

    def test_cached_subset_violation_rejected(self):
        frames = runner.parse_sse(gemini_text_stream())
        payload = json.loads(frames[-1]["data"])
        payload["usageMetadata"]["cachedContentTokenCount"] = 999
        frames[-1]["data"] = json.dumps(payload)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("cached" in f for f in failures))

    def test_duplicate_finish_rejected(self):
        frames = runner.parse_sse(gemini_text_stream())
        first = json.loads(frames[0]["data"])
        first["candidates"][0]["finishReason"] = "STOP"
        frames[0]["data"] = json.dumps(first)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("duplicate" in f for f in failures))

    def test_finish_only_on_terminal(self):
        frames = runner.parse_sse(gemini_text_stream())
        last = json.loads(frames[-1]["data"])
        del last["candidates"][0]["finishReason"]
        frames[-1]["data"] = json.dumps(last)
        first = json.loads(frames[0]["data"])
        first["candidates"][0]["finishReason"] = "STOP"
        frames[0]["data"] = json.dumps(first)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("terminal" in f for f in failures))

    def test_identity_change_rejected(self):
        for key, bad in (("responseId", "resp_other"),
                         ("modelVersion", "gemini-other")):
            with self.subTest(field=key):
                frames = runner.parse_sse(gemini_text_stream())
                payload = json.loads(frames[-1]["data"])
                payload[key] = bad
                frames[-1]["data"] = json.dumps(payload)
                failures = runner.check_gemini_integrity(frames, "client")
                self.assertTrue(any("changes" in f for f in failures),
                                f"identity change accepted: {key}")

    def test_tool_stream_exact_bigint_and_empty_args(self):
        frames = runner.parse_sse(gemini_tool_stream())
        self.assertEqual(runner.check_gemini_integrity(frames, "client"), [])
        ok = runner.check_gemini_integrity(
            frames, "client",
            {"0": {"tz": "UTC", "n": 9007199254740993}, "1": {}})
        self.assertEqual(ok, [])
        bad = runner.check_gemini_integrity(
            frames, "client", {"0": {"n": 1}})
        self.assertTrue(any("differ" in f for f in bad))
        # Exact large integers survive without float rounding.
        payload = json.loads(frames[1]["data"])
        args = payload["candidates"][0]["content"]["parts"][0][
            "functionCall"]["args"]
        self.assertEqual(args["n"], 9007199254740993)

    def test_parallel_tool_order_preserved(self):
        frames = runner.parse_sse(gemini_tool_stream())
        payload = json.loads(frames[1]["data"])
        parts = payload["candidates"][0]["content"]["parts"]
        self.assertEqual(
            [p["functionCall"]["name"] for p in parts],
            ["get_time", "get_weather"])

    def test_invalid_args_type_rejected(self):
        for bad_args in ('[1,2]', '"nope"', '42'):
            with self.subTest(args=bad_args):
                frames = runner.parse_sse(gemini_bad_tool_stream(bad_args))
                failures = runner.check_gemini_integrity(frames, "client")
                self.assertTrue(any("args" in f for f in failures),
                                f"bad args accepted: {bad_args}")

    def test_missing_call_name_rejected(self):
        frames = runner.parse_sse(gemini_bad_tool_stream('{}', name=""))
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("name" in f for f in failures))

    def test_duplicate_call_ids_rejected(self):
        frames = runner.parse_sse(gemini_tool_stream())
        payload = json.loads(frames[1]["data"])
        parts = payload["candidates"][0]["content"]["parts"]
        parts[1]["functionCall"]["id"] = parts[0]["functionCall"]["id"]
        frames[1]["data"] = json.dumps(payload)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("duplicate" in f for f in failures))

    def test_unknown_finish_rejected(self):
        frames = runner.parse_sse(gemini_text_stream())
        payload = json.loads(frames[-1]["data"])
        payload["candidates"][0]["finishReason"] = "OTHER"
        frames[-1]["data"] = json.dumps(payload)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("finishReason" in f for f in failures))

    def test_unknown_top_level_field_rejected(self):
        frames = runner.parse_sse(gemini_text_stream())
        payload = json.loads(frames[-1]["data"])
        payload["mystery"] = 1
        frames[-1]["data"] = json.dumps(payload)
        failures = runner.check_gemini_integrity(frames, "client")
        self.assertTrue(any("unexpected" in f for f in failures))

    def test_blocked_filter_finish_never_ordinary_stop(self):
        # A blocked prompt Honestly marked SAFETY passes; the same signal
        # with ordinary STOP is a contradiction, never a success.
        ok = runner.parse_sse(gemini_blocked_stream("SAFETY"))
        self.assertEqual(runner.check_gemini_integrity(ok, "client"), [])
        bad = runner.parse_sse(gemini_blocked_stream("STOP"))
        failures = runner.check_gemini_integrity(bad, "client")
        self.assertTrue(any("blocked" in f for f in failures))

    def test_malformed_shapes_never_crash(self):
        for body in (
                'data: {"candidates": "nope"}\n\n',
                'data: {"candidates": [{"content": "nope"}]}\n\n',
                'data: {"candidates": [{"content": {"role": "model", '
                '"parts": "nope"}"}]}\n\n',
                'data: {"usageMetadata": "nope"}\n\n',
                'data: [1,2]\n\n',
                'data: not-json\n\n'):
            with self.subTest(body=body[:40]):
                frames = runner.parse_sse(body)
                try:
                    failures = runner.check_gemini_integrity(frames, "client")
                except Exception as e:  # never crash on malformed shapes
                    self.fail(f"validator crashed: {e}")
                self.assertTrue(failures)


class GeminiArrayTest(unittest.TestCase):
    def test_official_shaped_array_passes(self):
        body = gemini_array_body()
        spec_ok = runner.check_gemini_array_body(
            body, "client",
            {"0": {"tz": "UTC", "n": 9007199254740993}},
            "ab")
        self.assertEqual(spec_ok, [])

    def test_truncated_array_rejected(self):
        body = gemini_array_body()
        failures = runner.check_gemini_array_body(body[:-5], "client")
        self.assertTrue(any("truncat" in f or "missing" in f for f in failures))
        # A failed array is never valid closed JSON.
        with self.assertRaises(ValueError):
            json.loads(body[:-5])

    def test_trailing_comma_array_rejected(self):
        body = gemini_array_body().replace("}]", "},]")
        failures = runner.check_gemini_array_body(body, "client")
        self.assertTrue(any("invalid" in f or "closed" in f for f in failures))
        with self.assertRaises(ValueError):
            json.loads(body)

    def test_empty_array_rejected(self):
        failures = runner.check_gemini_array_body("[]", "client")
        self.assertTrue(any("empty" in f for f in failures))


class FixtureValidityTest(unittest.TestCase):
    """Every scripted upstream SSE `data:` payload must be valid JSON.

    A typo in a scripted frame (e.g. a stray quote inside a fragmented
    tool-argument frame) fails the translation closed instead of exercising
    the intended path, so the fixtures are parsed here. Frames that
    deliberately script invalid bytes are allowlisted per (case, frame
    index), and the allowlist entries are asserted invalid so the list
    cannot go stale.
    """
    CASES_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cases")
    DELIBERATELY_INVALID = {
        # Invalid frame after one valid chunk: post-commit abort path.
        ("chat_to_gemini_sse_midstream_invalid", 1),
    }

    def test_scripted_sse_data_parses(self):
        names = sorted(f for f in os.listdir(self.CASES_DIR) if f.endswith(".json"))
        self.assertTrue(names, "no case files found")
        checked = 0
        for name in names:
            with open(os.path.join(self.CASES_DIR, name)) as fh:
                case = json.load(fh)
            frames = (case.get("upstream") or {}).get("sse_frames") or []
            for i, fr in enumerate(frames):
                for line in str(fr).splitlines():
                    s = line.strip()
                    if not s.startswith("data:"):
                        continue
                    payload = s[len("data:"):].strip()
                    if payload in ("", "[DONE]"):
                        continue
                    key = (case.get("name", name), i)
                    if key in self.DELIBERATELY_INVALID:
                        with self.assertRaises(
                                ValueError,
                                msg=f"{name} frame {i} must stay deliberately invalid"):
                            json.loads(payload)
                    else:
                        try:
                            json.loads(payload)
                        except ValueError as e:
                            self.fail(f"{name} frame {i} is not valid JSON: "
                                      f"{payload[:120]!r} ({e})")
                        checked += 1
        self.assertGreater(checked, 0, "no scripted SSE payloads checked")


if __name__ == "__main__":
    unittest.main()
