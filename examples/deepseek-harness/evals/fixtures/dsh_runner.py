#!/usr/bin/env python3
"""Bridge skill-up's Custom Engine contract to DSH's headless CLI."""

from __future__ import annotations

import argparse
import json
import os
import re
import signal
import subprocess
import sys
import time
import uuid
from pathlib import Path
from typing import Any


def yaml_string(value: str) -> str:
    """Return a JSON string, which is also a valid YAML scalar."""
    return json.dumps(value, ensure_ascii=False)


def safe_segment(value: str, fallback: str) -> str:
    value = re.sub(r"[^A-Za-z0-9._-]+", "-", value).strip("-.")
    return value or fallback


def output_text(value: str | bytes | None) -> str:
    if value is None:
        return ""
    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace")
    return value


def redact_secrets(value: str) -> str:
    for name in ("DSH_API_KEY", "DASHSCOPE_EVAL_API_KEY"):
        secret = os.environ.get(name, "")
        if secret:
            value = value.replace(secret, "[REDACTED]")
    return value


def redact_data(value: Any) -> Any:
    if isinstance(value, str):
        return redact_secrets(value)
    if isinstance(value, list):
        return [redact_data(item) for item in value]
    if isinstance(value, dict):
        return {
            redact_secrets(key) if isinstance(key, str) else key: redact_data(item)
            for key, item in value.items()
        }
    return value


def sanitize_run_artifacts(root: Path) -> None:
    secrets = {
        value.encode()
        for name in ("DSH_API_KEY", "DASHSCOPE_EVAL_API_KEY")
        if (value := os.environ.get(name, ""))
    }
    if not secrets:
        return
    for path in root.rglob("*"):
        if not path.is_file():
            continue
        content = path.read_bytes()
        sanitized = content
        for secret in secrets:
            sanitized = sanitized.replace(secret, b"[REDACTED]")
        if sanitized != content:
            path.write_bytes(sanitized)


def terminate_process_tree(process: subprocess.Popen[str]) -> None:
    if os.name == "nt":
        subprocess.run(
            ["taskkill", "/PID", str(process.pid), "/T", "/F"],
            capture_output=True,
            check=False,
        )
        return
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        return


def run_dsh(
    command: list[str], workspace: Path, env: dict[str, str], timeout: float
) -> tuple[int, str, str, bool]:
    process_options: dict[str, Any] = {}
    if os.name == "nt":
        process_options["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
    else:
        process_options["start_new_session"] = True
    process = subprocess.Popen(
        command,
        cwd=workspace,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        **process_options,
    )
    try:
        stdout, stderr = process.communicate(timeout=timeout)
        return process.returncode, stdout, stderr, False
    except subprocess.TimeoutExpired:
        terminate_process_tree(process)
        stdout, stderr = process.communicate()
        return 124, stdout, stderr, True


def conversation_prompt(messages: list[dict[str, Any]]) -> str:
    if len(messages) == 1 and messages[0].get("role") == "user":
        return str(messages[0].get("content", ""))
    rendered = [
        "Continue the following conversation. Treat earlier assistant messages as context, "
        "then answer the final user message."
    ]
    for message in messages:
        role = str(message.get("role", "user")).upper()
        rendered.append(f"\n{role}:\n{message.get('content', '')}")
    return "\n".join(rendered)


def write_dsh_config(home: Path, session_root: Path, skills_dir: Path) -> None:
    model = os.environ.get("DSH_MODEL", "qwen3.8-max")
    base_url = os.environ.get(
        "DSH_BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1"
    )
    patch_lines = [
        "- id: agent-default-model",
        "  config:",
        "    provider: skill-up-openai",
        f"    model: {yaml_string(model)}",
        "- id: session-persistence-jsonl",
        "  config:",
        f"    root: {yaml_string(str(session_root))}",
        "    compression: none",
        "    packChunks: false",
        "- id: session-title-llm",
        "  disabled: true",
    ]
    if skills_dir.is_dir():
        patch_lines.extend(
            [
                "- id: skill-filesystem",
                "  config:",
                "    includeDefaultRoots: false",
                "    customSkillDirs:",
                f"      - {yaml_string(str(skills_dir))}",
            ]
        )
    (home / "cordis.patch.yml").write_text("\n".join(patch_lines) + "\n")

    settings = "\n".join(
        [
            "llm-pi-ai:",
            "  providers:",
            "    skill-up-openai:",
            "      apiKeyEnv: DSH_API_KEY",
            "      api: openai-completions",
            f"      baseURL: {yaml_string(base_url)}",
            "      models:",
            f"        - id: {yaml_string(model)}",
            "",
        ]
    )
    (home / "settings.yaml").write_text(settings)


def transcript(messages: list[dict[str, Any]], final_message: str) -> list[dict[str, Any]]:
    result = []
    current_turn = 1
    for message in messages:
        role = str(message.get("role", "user"))
        if role == "user" and result:
            current_turn += 1
        result.append(
            {
                "role": role,
                "content": redact_secrets(str(message.get("content", ""))),
                "turn": current_turn,
            }
        )
    result.append({"role": "assistant", "content": final_message, "turn": current_turn})
    return result


def content_text(blocks: Any) -> str:
    if not isinstance(blocks, list):
        return ""
    parts = []
    for block in blocks:
        if not isinstance(block, dict):
            continue
        if block.get("type") == "text" and block.get("text"):
            parts.append(redact_secrets(str(block["text"])))
        elif block.get("type") == "tool-result":
            nested = content_text(block.get("content"))
            if nested:
                parts.append(nested)
    return "\n".join(parts)


def parse_arguments(raw: Any) -> dict[str, Any]:
    if isinstance(raw, dict):
        return redact_data(raw)
    if not isinstance(raw, str) or not raw:
        return {}
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError:
        return {"_raw": redact_secrets(raw)}
    value = parsed if isinstance(parsed, dict) else {"_raw": raw}
    return redact_data(value)


def read_session(
    session_file: Path | None,
    input_messages: list[dict[str, Any]],
    final_message: str,
) -> tuple[str, list[dict[str, Any]], int, int, int]:
    if session_file is None:
        fallback = transcript(input_messages, final_message)
        return "", fallback, 0, 0, max(1, len(input_messages))

    session_id = session_file.stem
    events: list[dict[str, Any]] = []
    input_tokens = 0
    output_tokens = 0
    turns = 1
    for line in session_file.read_text().splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(event, dict):
            continue
        if event.get("type") == "session":
            session_id = str(event.get("id") or session_id)
            continue
        data = event.get("data")
        if not isinstance(data, dict):
            continue
        turn = int(data.get("turn") or 1)
        turns = max(turns, turn)
        if event.get("type") in ("assistant/message", "assistant/attempt"):
            usage = data.get("usage") or {}
            message = data.get("message") or {}
            blocks = message.get("content") if isinstance(message, dict) else []
            if event.get("type") == "assistant/attempt":
                blocks = []
                for stream_item in data.get("stream") or []:
                    if not isinstance(stream_item, dict) or stream_item.get("type") != "chunk":
                        continue
                    chunk = stream_item.get("chunk") or {}
                    if not isinstance(chunk, dict):
                        continue
                    if chunk.get("type") == "usage":
                        usage = chunk.get("usage") or usage
                    elif chunk.get("type") == "block-end" and isinstance(
                        chunk.get("block"), dict
                    ):
                        blocks.append(chunk["block"])
            if isinstance(usage, dict):
                input_tokens += sum(
                    int(usage.get(key) or 0)
                    for key in ("inputTokens", "cacheReadTokens", "cacheWriteTokens")
                )
                output_tokens += int(usage.get("outputTokens") or 0)
            for block in blocks if isinstance(blocks, list) else []:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == "text" and block.get("text"):
                    events.append(
                        {
                            "role": "assistant",
                            "content": redact_secrets(str(block["text"])),
                            "turn": turn,
                        }
                    )
                elif block.get("type") == "tool-call":
                    events.append(
                        {
                            "role": "tool_call",
                            "turn": turn,
                            "tool_call": {
                                "id": str(block.get("id") or ""),
                                "name": str(block.get("name") or ""),
                                "arguments": parse_arguments(block.get("arguments")),
                            },
                        }
                    )
        elif event.get("type") == "tool/result":
            message = data.get("message") or {}
            blocks = message.get("content") if isinstance(message, dict) else []
            for block in blocks if isinstance(blocks, list) else []:
                if not isinstance(block, dict) or block.get("type") != "tool-result":
                    continue
                events.append(
                    {
                        "role": "tool_result",
                        "turn": turn,
                        "content": content_text(block.get("content")),
                        "tool_result": {
                            "call_id": str(block.get("toolCallId") or ""),
                            "status": "error" if data.get("error") else "success",
                            "content": content_text(block.get("content")),
                        },
                    }
                )

    normalized_inputs = []
    current_turn = 1
    for message in input_messages:
        role = str(message.get("role", "user"))
        if role == "user" and normalized_inputs:
            current_turn += 1
        normalized_inputs.append(
            {
                "role": role,
                "content": redact_secrets(str(message.get("content", ""))),
                "turn": current_turn,
            }
        )
    combined = normalized_inputs + events
    if not any(
        item.get("role") == "assistant" and item.get("content", "").strip() == final_message
        for item in combined
    ):
        combined.append({"role": "assistant", "content": final_message, "turn": turns})
    return session_id, combined, input_tokens, output_tokens, turns


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    request = json.loads(Path(args.input).read_text())
    workspace = Path(request["workspace"]).resolve()
    run_root = workspace / ".skill-up-dsh" / "runs"
    run_root.mkdir(parents=True, exist_ok=True)
    run_id = "-".join(
        [
            safe_segment(str(request.get("case_id", "case")), "case"),
            safe_segment(str(request.get("variant", "run")), "run"),
            uuid.uuid4().hex[:12],
        ]
    )
    home = run_root / run_id
    session_root = home / "sessions"
    home.mkdir(parents=True)
    session_root.mkdir()
    skills_dir = Path(os.environ.get("DSH_SKILLS_DIR", workspace / ".dsh-skills"))
    write_dsh_config(home, session_root, skills_dir)

    messages = request.get("messages") or []
    prompt = conversation_prompt(messages)
    timeout = float(request.get("timeout_seconds") or 300)
    timeout_slack = min(5.0, max(0.5, timeout * 0.1))
    dsh_timeout = max(0.1, timeout - timeout_slack)
    env = os.environ.copy()
    env["DSH_HOME"] = str(home)
    env.setdefault("DSH_TELEMETRY_MODE", "DISABLED")
    env.setdefault("DSH_PERMISSION_MODE", "danger-full-access")

    started = time.monotonic()
    exit_code, stdout, raw_stderr, timed_out = run_dsh(
        [
            env.get("DSH_BIN", "dsh"),
            "--profile",
            "headless",
            "--",
            "--",
            prompt,
        ],
        workspace,
        env,
        dsh_timeout,
    )
    final_message = redact_secrets(output_text(stdout).strip())
    stderr = redact_secrets(output_text(raw_stderr))
    if exit_code != 0 and not stderr.strip():
        stderr = final_message
    if timed_out:
        stderr += f"\nDSH timed out after {dsh_timeout:g}s"

    duration_ms = int((time.monotonic() - started) * 1000)
    stderr_path = home / "stderr.log"
    stderr_path.write_text(stderr)
    sanitize_run_artifacts(home)
    generated_files = [
        str(path.relative_to(workspace))
        for path in sorted(home.rglob("*"))
        if path.is_file()
    ]
    session_files = sorted(session_root.rglob("*.jsonl"))
    session_file = session_files[-1] if session_files else None
    session_id, run_transcript, input_tokens, output_tokens, turns = read_session(
        session_file, messages, final_message
    )

    result = {
        "engine": "deepseek-harness",
        "model": os.environ.get("DSH_MODEL", "qwen3.8-max"),
        "session_id": session_id,
        "exit_code": exit_code,
        "duration_ms": duration_ms,
        "turns": turns,
        "input_tokens": input_tokens,
        "output_tokens": output_tokens,
        "final_message": final_message,
        "stderr": stderr if exit_code else "",
        "transcript": run_transcript,
        "artifacts": {
            "generated_files": generated_files,
            "logs": str(stderr_path.relative_to(workspace)),
        },
    }
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    # The wrapper completed its transport contract once SessionResult was
    # written. DSH failures stay non-zero in the structured result so skill-up
    # can report their redacted diagnostics instead of replacing them with an
    # empty command-level error.
    return 0


if __name__ == "__main__":
    sys.exit(main())
