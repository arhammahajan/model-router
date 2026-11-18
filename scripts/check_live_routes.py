"""Smoke-test selected live model routes with small text requests."""

from __future__ import annotations

import argparse
import json
import os
import re
import urllib.error
import urllib.parse
import urllib.request
import uuid

PROMPT = "Reply with exactly OK. Do not use tools."
MAX_REPLY_BYTES = 4 << 20
DIALECTS = ("chat", "responses", "anthropic", "gemini")
ANTHROPIC_SUCCESS_REASONS = ("end_turn", "stop_sequence")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def parse_route(value: str) -> tuple[str, str]:
    dialect, _, model = value.partition("=")
    if dialect not in DIALECTS or not model:
        raise argparse.ArgumentTypeError("expected chat|responses|anthropic|gemini=MODEL")
    return dialect, model


def request_body(dialect: str, model: str, stream: bool, max_tokens: int) -> tuple[str, dict]:
    messages = [{"role": "user", "content": PROMPT}]
    if dialect == "responses":
        return "/v1/responses", {
            "model": model, "input": PROMPT, "store": False,
            "stream": stream, "max_output_tokens": max_tokens,
        }
    if dialect == "gemini":
        method = "streamGenerateContent?alt=sse" if stream else "generateContent"
        model_path = urllib.parse.quote(model, safe="")
        return f"/v1beta/models/{model_path}:{method}", {
            "contents": [{"role": "user", "parts": [{"text": PROMPT}]}],
            "generationConfig": {"maxOutputTokens": max_tokens},
        }
    body = {"model": model, "messages": messages, "stream": stream, "max_tokens": max_tokens}
    if dialect == "anthropic":
        return "/v1/messages", body
    if stream:
        body["stream_options"] = {"include_usage": True}
    return "/v1/chat/completions", body


def model_text_and_finish(dialect: str, reply: dict) -> tuple[bool, bool]:
    if dialect == "chat":
        choice = reply["choices"][0]
        return bool(choice["message"].get("content")), choice.get("finish_reason") == "stop"
    if dialect == "responses":
        has_text = any(
            p.get("type") == "output_text" and p.get("text")
            for item in reply["output"] for p in item.get("content", [])
        )
        return has_text, reply["status"] == "completed"
    if dialect == "anthropic":
        has_text = any(p.get("type") == "text" and p.get("text") for p in reply["content"])
        return has_text, reply.get("stop_reason") in ANTHROPIC_SUCCESS_REASONS
    candidate = reply["candidates"][0]
    return gemini_has_text(candidate), candidate.get("finishReason") == "STOP"


def gemini_has_text(candidate: dict) -> bool:
    return any(
        p.get("text") and not p.get("thought")
        for p in candidate.get("content", {}).get("parts", [])
    )


def stream_text_and_finish(dialect: str, raw: bytes) -> tuple[bool, bool]:
    text = terminal = finish = False
    # Join data lines within each SSE frame so pretty-printed JSON remains valid.
    for frame in raw.replace(b"\r\n", b"\n").split(b"\n\n"):
        data = b"\n".join(line[5:].lstrip() for line in frame.splitlines() if line.startswith(b"data:"))
        if not data:
            continue
        if data == b"[DONE]":
            terminal = terminal or dialect == "chat"
            continue
        event = json.loads(data)
        event_type = event.get("type")
        if event.get("error") or event_type in ("error", "response.failed", "response.incomplete"):
            return text, False
        if dialect == "chat":
            for choice in event.get("choices", []):
                text = text or bool(choice.get("delta", {}).get("content"))
                if choice.get("finish_reason") is not None:
                    finish = choice["finish_reason"] == "stop"
        elif dialect == "responses":
            text = text or (event_type == "response.output_text.delta" and bool(event.get("delta")))
            terminal = terminal or event_type == "response.completed"
        elif dialect == "anthropic":
            delta = event.get("delta", {})
            text = text or (delta.get("type") == "text_delta" and bool(delta.get("text")))
            if event_type == "message_delta" and delta.get("stop_reason") is not None:
                finish = delta["stop_reason"] in ANTHROPIC_SUCCESS_REASONS
            terminal = terminal or event_type == "message_stop"
        else:
            for candidate in event.get("candidates", []):
                text = text or gemini_has_text(candidate)
                if candidate.get("finishReason") is not None:
                    terminal = candidate["finishReason"] == "STOP"
    return text, terminal and (finish or dialect not in ("chat", "anthropic"))


def safe_error_code(raw: bytes) -> str:
    try:
        error = json.loads(raw).get("error", {})
        details = error.get("details", [])
        value = error.get("code") or error.get("type")
        if details and isinstance(details[0], dict):
            value = details[0].get("reason", value)
        if isinstance(value, str) and re.fullmatch(r"[A-Za-z_-]{1,80}", value):
            return value
    except (ValueError, AttributeError, TypeError):
        pass
    return "request failed"


def probe(opener, args, dialect: str, model: str, stream: bool, key: str) -> str | None:
    path, body = request_body(dialect, model, stream, args.max_tokens)
    headers = {
        "Content-Type": "application/json",
        "x-session-id": str(uuid.uuid4()),
    }
    if key:
        headers["Authorization"] = f"Bearer {key}"
    if dialect == "anthropic":
        headers["anthropic-version"] = "2023-06-01"
    req = urllib.request.Request(args.origin + path, json.dumps(body).encode(), headers)
    try:
        with opener.open(req, timeout=args.timeout) as response:
            media = response.headers.get_content_type()
            raw = response.read(MAX_REPLY_BYTES + 1)
        if len(raw) > MAX_REPLY_BYTES:
            return "response exceeded the smoke-check size bound"
        if stream:
            if media != "text/event-stream":
                return "expected an SSE response"
            has_text, finished = stream_text_and_finish(dialect, raw)
        else:
            has_text, finished = model_text_and_finish(dialect, json.loads(raw))
        if not finished:
            return "missing successful completion"
        if not has_text:
            return "missing visible text"
    except urllib.error.HTTPError as e:
        return f"HTTP {e.code} ({safe_error_code(e.read(65536))})"
    except (OSError, ValueError, KeyError, IndexError, TypeError, AttributeError) as e:
        return f"invalid or unavailable response ({type(e).__name__})"
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", required=True, help="router origin, such as http://127.0.0.1:18080")
    parser.add_argument("--route", action="append", required=True, type=parse_route, metavar="DIALECT=MODEL")
    parser.add_argument("--mode", choices=("json", "sse", "both"), default="both")
    parser.add_argument("--key-env", help="read the router API key from this environment variable")
    parser.add_argument("--timeout", type=float, default=60, help="socket timeout, in seconds")
    parser.add_argument("--max-tokens", type=int, default=256, help="output token cap per request")
    args = parser.parse_args()
    origin = urllib.parse.urlsplit(args.origin)
    if (origin.scheme not in ("http", "https") or not origin.hostname or origin.username is not None
            or origin.path not in ("", "/") or origin.query or origin.fragment):
        parser.error("--origin must be an HTTP(S) origin")
    if not 0 < args.timeout <= 120:
        parser.error("--timeout must be in (0, 120]")
    if not 1 <= args.max_tokens <= 1024:
        parser.error("--max-tokens must be between 1 and 1024")
    args.origin = args.origin.rstrip("/")
    key = os.environ.get(args.key_env, "") if args.key_env else ""
    if args.key_env and not key:
        parser.error("the environment variable named by --key-env is empty")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    modes = ("json", "sse") if args.mode == "both" else (args.mode,)
    failures = 0
    for dialect, model in args.route:
        for mode in modes:
            problem = probe(opener, args, dialect, model, mode == "sse", key)
            failures += problem is not None
            outcome = f"FAIL ({problem})" if problem else "ok"
            print(f"{dialect} {model} {mode} {outcome}", flush=True)
    return int(bool(failures))


if __name__ == "__main__":
    raise SystemExit(main())
