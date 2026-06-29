#!/usr/bin/env python3
# scripts/extract_chat.py — 从 logs/chat_io/<YYYY-MM-DD>.log 提取对话原文
#
# 用法:
#   # 按 log_id 提取单条对话（自动从 MySQL 查 created_at 定位文件）
#   scripts/extract_chat.py --log-id 3722
#
#   # 按 log_id 并指定日期文件（跳过 DB 查询，离线可用）
#   scripts/extract_chat.py --log-id 3722 --date 2026-06-29
#
#   # 批量提取某天所有对话
#   scripts/extract_chat.py --date 2026-06-30
#
#   # 批量提取并输出到文件
#   scripts/extract_chat.py --date 2026-06-30 --out /tmp/chats.txt
#
#   # 只看某个 key 的对话（先解析 chat_logs 拿 auth_key_id 过滤）
#   scripts/extract_chat.py --date 2026-06-30 --auth-key-id 7
#
# 环境变量:
#   DB_DRIVER / DATABASE_URL — 与服务同款配置；只在使用 --log-id 自动定位时需要。
#
# 输出格式: 纯文本，按时间顺序列出每条对话的元信息 + user/assistant 原文。

import argparse
import json
import os
import re
import sys
from datetime import datetime
from pathlib import Path

CHAT_IO_DIR = Path("logs/chat_io")


# ---------- 文件读取 ----------

def read_records(path: Path):
    """从 chat_io 日志文件按行解析 (log_id, type, content) 记录。

    每条记录占两行: 头部 JSON + 原文行。原文行可能很长，按头部 length 读取。
    """
    with open(path, "rb") as f:
        data = f.read()
    pos = 0
    n = len(data)
    while pos < n:
        nl = data.find(b"\n", pos)
        if nl == -1:
            break
        header_raw = data[pos:nl]
        pos = nl + 1
        try:
            header = json.loads(header_raw)
        except json.JSONDecodeError:
            # 头部损坏，跳过这一行尝试恢复
            continue
        length = int(header.get("length", 0))
        if length <= 0:
            # 无内容，跳过
            content = ""
            # 仍要消费内容行（如果有）
            if pos < n and data[pos:pos+1] != b"\n":
                nl2 = data.find(b"\n", pos)
                if nl2 == -1:
                    pos = n
                else:
                    pos = nl2 + 1
            yield header, ""
            continue
        content = data[pos:pos+length]
        pos += length
        # 消费内容后的换行
        if pos < n and data[pos:pos+1] == b"\n":
            pos += 1
        yield header, content.decode("utf-8", errors="replace")


def collect_by_log_id(path: Path, target_log_id=None):
    """返回 {log_id: {"input": str, "output": str, "created_at": datetime}}"""
    result = {}
    for header, content in read_records(path):
        log_id = header.get("log_id")
        if target_log_id is not None and log_id != target_log_id:
            continue
        slot = result.setdefault(log_id, {"input": "", "output": "", "created_at": header.get("created_at")})
        rtype = header.get("type")
        if rtype == "input":
            slot["input"] = content
        elif rtype == "output":
            slot["output"] = content
        ts = header.get("created_at")
        if ts:
            slot["created_at"] = ts
    return result


# ---------- 对话原文提取 ----------

def extract_text_from_value(v):
    """从可能是 str / list / dict 的 content 字段提取纯文本。"""
    if v is None:
        return ""
    if isinstance(v, str):
        return v
    if isinstance(v, list):
        parts = []
        for item in v:
            parts.append(extract_text_from_value(item))
        return "\n".join(p for p in parts if p)
    if isinstance(v, dict):
        # OpenAI: {"type":"text","text":"..."}
        # Anthropic: {"type":"text","text":"..."} / {"type":"thinking","thinking":"..."}
        # Gemini: {"text":"..."}
        for key in ("text", "thinking", "content"):
            if key in v and isinstance(v[key], str):
                prefix = ""
                if v.get("type") == "thinking":
                    prefix = "[thinking] "
                return prefix + v[key]
        # 兜底：递归取值
        return extract_text_from_value(list(v.values()))
    return str(v)


def parse_openai_input(body: dict):
    """OpenAI 请求: {model, messages:[{role, content}]}"""
    messages = body.get("messages", [])
    out = []
    for m in messages:
        role = m.get("role", "?")
        content = extract_text_from_value(m.get("content"))
        out.append((role, content))
    return body.get("model", ""), out


def parse_anthropic_input(body: dict):
    """Anthropic 请求: {model, system, messages:[{role, content}]}"""
    messages = body.get("messages", [])
    out = []
    sys_msg = body.get("system")
    if sys_msg:
        out.append(("system", extract_text_from_value(sys_msg)))
    for m in messages:
        role = m.get("role", "?")
        content = extract_text_from_value(m.get("content"))
        out.append((role, content))
    return body.get("model", ""), out


def parse_gemini_input(body: dict):
    """Gemini 请求: {contents:[{role, parts:[{text}]}]}"""
    contents = body.get("contents", [])
    out = []
    for c in contents:
        role = c.get("role", "?")
        if role == "user":
            role = "user"
        elif role == "model":
            role = "assistant"
        parts = c.get("parts", [])
        text = "\n".join(extract_text_from_value(p) for p in parts)
        out.append((role, text))
    return body.get("model", body.get("model_id", "")), out


def detect_and_parse_input(raw: str):
    """识别请求风格并解析，返回 (model, [(role, text), ...])"""
    try:
        body = json.loads(raw)
    except json.JSONDecodeError:
        return "", [("raw", raw)]
    if "messages" in body:
        # OpenAI / Anthropic 同款 messages 结构；Anthropic 多半有 system 顶层字段
        if "system" in body and body.get("system"):
            return parse_anthropic_input(body)
        return parse_openai_input(body)
    if "contents" in body:
        return parse_gemini_input(body)
    # 兜底
    return body.get("model", ""), [("raw", json.dumps(body, ensure_ascii=False)[:500])]


def parse_output(raw: str):
    """解析响应字符串，返回 [(role, text), ...]。

    raw 是 OutputUnion 序列化后的 JSON: {"OfString":"","OfStringArray":[...]}。
    OfString 存放普通响应（OpenAI/Anthropic/Gemini JSON），OfStringArray 存放 SSE chunks。
    """
    if not raw:
        return []
    try:
        ou = json.loads(raw)
    except json.JSONDecodeError:
        return [("assistant", raw)]

    of_string = ou.get("OfString", "")
    of_array = ou.get("OfStringArray") or []

    # 流式优先：从 chunks 还原完整响应
    if of_array:
        text_chunks = []
        for chunk in of_array:
            try:
                ev = json.loads(chunk)
            except json.JSONDecodeError:
                # 可能是 "data: {...}\n\n" 形式
                m = re.search(r"data:\s*(\{.*\})", chunk)
                if not m:
                    continue
                try:
                    ev = json.loads(m.group(1))
                except json.JSONDecodeError:
                    continue
            text = extract_event_text(ev)
            if text:
                text_chunks.append(text)
        if text_chunks:
            return [("assistant", "\n".join(text_chunks))]

    # 非流式响应
    if of_string:
        try:
            resp = json.loads(of_string)
        except json.JSONDecodeError:
            return [("assistant", of_string)]
        return parse_response_obj(resp)
    return []


def extract_event_text(ev: dict):
    """从单个 SSE chunk 事件里抽文本片段。"""
    # OpenAI: {choices:[{delta:{content:"..."}}]}
    choices = ev.get("choices")
    if choices:
        parts = []
        for c in choices:
            delta = c.get("delta", {}) or c.get("message", {})
            t = delta.get("content") or delta.get("reasoning_content")
            if t:
                parts.append(t)
        return "".join(parts)
    # Anthropic: {type:"content_block_delta", delta:{type:"text_delta", text:"..."}}
    if ev.get("type") == "content_block_delta":
        delta = ev.get("delta", {})
        if delta.get("type") == "text_delta":
            return delta.get("text", "")
        if delta.get("type") == "thinking_delta":
            return "[thinking] " + delta.get("thinking", "")
        return ""
    # Gemini: {candidates:[{content:{parts:[{text}]}}]}
    candidates = ev.get("candidates")
    if candidates:
        parts = []
        for c in candidates:
            parts.append(extract_text_from_value(c.get("content", {}).get("parts", [])))
        return "\n".join(p for p in parts if p)
    return ""


def parse_response_obj(resp: dict):
    """解析非流式响应 JSON，返回 [(role, text), ...]。"""
    # OpenAI: {choices:[{message:{role, content}}]}
    if "choices" in resp:
        out = []
        for c in resp.get("choices", []):
            msg = c.get("message", {})
            role = msg.get("role", "assistant")
            text = extract_text_from_value(msg.get("content"))
            if text:
                out.append((role, text))
        return out
    # Anthropic: {content:[{type, text/thinking}]}
    if "content" in resp:
        out = []
        for blk in resp.get("content", []):
            t = blk.get("type")
            if t == "text":
                out.append(("assistant", blk.get("text", "")))
            elif t == "thinking":
                out.append(("thinking", blk.get("thinking", "")))
        return out
    # Gemini: {candidates:[{content:{parts:[{text}]}}]}
    if "candidates" in resp:
        out = []
        for c in resp.get("candidates", []):
            text = extract_text_from_value(c.get("content", {}).get("parts", []))
            if text:
                out.append(("assistant", text))
        return out
    return [("assistant", json.dumps(resp, ensure_ascii=False)[:500])]


# ---------- DB 查询 ----------

def find_log_date_from_db(log_id: int) -> str:
    """通过 MySQL/SQLite 查 chat_logs.created_at 定位文件日期。"""
    driver = os.environ.get("DB_DRIVER", "sqlite")
    if driver == "mysql":
        dsn = os.environ.get("DATABASE_URL", "")
        if not dsn:
            sys.exit("DATABASE_URL 未设置；--log-id 自动定位需要 DB_DRIVER=mysql + DATABASE_URL")
        import pymysql
        # 解析 user:pass@tcp(host:port)/db?...
        m = re.match(r"([^:]+):([^@]+)@tcp\(([^:]+):(\d+)\)/([^?]+)", dsn)
        if not m:
            sys.exit(f"无法解析 DATABASE_URL: {dsn}")
        user, pwd, host, port, dbname = m.groups()
        conn = pymysql.connect(host=host, port=int(port), user=user, password=pwd, database=dbname, charset="utf8mb4")
        try:
            with conn.cursor() as cur:
                cur.execute("SELECT created_at FROM chat_logs WHERE id=%s", (log_id,))
                row = cur.fetchone()
        finally:
            conn.close()
    else:
        import sqlite3
        conn = sqlite3.connect("db/llmio.db")
        try:
            cur = conn.execute("SELECT created_at FROM chat_logs WHERE id=?", (log_id,))
            row = cur.fetchone()
        finally:
            conn.close()
    if not row:
        sys.exit(f"log_id={log_id} 在数据库中未找到")
    ts = row[0]
    if isinstance(ts, str):
        ts = datetime.fromisoformat(ts.replace("Z", "+00:00"))
    return ts.strftime("%Y-%m-%d")


# ---------- 输出渲染 ----------

def render_conversation(log_id, rec, meta=None):
    """把一条 chat_io 记录渲染成可读文本块。"""
    lines = []
    meta_str = ""
    if meta:
        meta_str = " | ".join(f"{k}={v}" for k, v in meta.items() if v is not None and v != "")
    lines.append(f"=== log_id={log_id}  created_at={rec.get('created_at','?')}")
    if meta_str:
        lines.append(f"    {meta_str}")
    lines.append("")

    model, input_msgs = detect_and_parse_input(rec.get("input", ""))
    if model:
        lines.append(f"model: {model}")
    for role, text in input_msgs:
        lines.append(f"[{role}]")
        lines.append(text.rstrip() if text else "(empty)")
        lines.append("")

    outputs = parse_output(rec.get("output", ""))
    if not outputs:
        lines.append("[assistant]")
        lines.append("(no output)")
        lines.append("")
    else:
        for role, text in outputs:
            lines.append(f"[{role}]")
            lines.append(text.rstrip() if text else "(empty)")
            lines.append("")

    lines.append("-" * 60)
    return "\n".join(lines)


def fetch_log_meta(log_ids):
    """从 DB 查 chat_logs 的 name/style/auth_key_id 等元信息，返回 {log_id: meta}。"""
    if not log_ids:
        return {}
    driver = os.environ.get("DB_DRIVER", "sqlite")
    meta = {}
    if driver == "mysql":
        dsn = os.environ.get("DATABASE_URL", "")
        if not dsn:
            return {}
        import pymysql
        m = re.match(r"([^:]+):([^@]+)@tcp\(([^:]+):(\d+)\)/([^?]+)", dsn)
        if not m:
            return {}
        user, pwd, host, port, dbname = m.groups()
        conn = pymysql.connect(host=host, port=int(port), user=user, password=pwd, database=dbname, charset="utf8mb4")
        try:
            with conn.cursor() as cur:
                ids = ",".join(str(i) for i in log_ids)
                cur.execute(f"SELECT id, name, style, provider_model, auth_key_id FROM chat_logs WHERE id IN ({ids})")
                for row in cur.fetchall():
                    meta[row[0]] = {"name": row[1], "style": row[2], "provider_model": row[3], "auth_key_id": row[4]}
        finally:
            conn.close()
    else:
        import sqlite3
        conn = sqlite3.connect("db/llmio.db")
        try:
            ids = ",".join(str(i) for i in log_ids)
            for row in conn.execute(f"SELECT id, name, style, provider_model, auth_key_id FROM chat_logs WHERE id IN ({ids})"):
                meta[row[0]] = {"name": row[1], "style": row[2], "provider_model": row[3], "auth_key_id": row[4]}
        finally:
            conn.close()
    return meta


# ---------- 主入口 ----------

def main():
    ap = argparse.ArgumentParser(description="从 chat_io 文件提取对话原文")
    ap.add_argument("--log-id", type=int, help="提取指定 log_id 的对话")
    ap.add_argument("--date", help="日期 YYYY-MM-DD；指定时按该日文件批量提取")
    ap.add_argument("--auth-key-id", type=int, help="配合 --date 使用：只输出该 auth_key_id 的对话")
    ap.add_argument("--out", help="输出到文件，默认 stdout")
    args = ap.parse_args()

    if args.log_id is None and not args.date:
        ap.error("至少指定 --log-id 或 --date 中的一个")

    # 确定目标日期文件
    if args.log_id is not None and not args.date:
        date = find_log_date_from_db(args.log_id)
    else:
        date = args.date

    path = CHAT_IO_DIR / f"{date}.log"
    if not path.exists():
        sys.exit(f"chat_io 文件不存在: {path}")

    # 读取该文件所有记录
    by_log = collect_by_log_id(path, target_log_id=args.log_id)
    if not by_log:
        sys.exit(f"未在 {path} 中找到 log_id={args.log_id} 的记录" if args.log_id else f"{path} 中无记录")

    # 过滤 auth_key_id（需要查 DB 拿元信息）
    log_ids = sorted(by_log.keys())
    meta = fetch_log_meta(log_ids) if args.auth_key_id else fetch_log_meta(log_ids)
    if args.auth_key_id:
        log_ids = [lid for lid in log_ids if meta.get(lid, {}).get("auth_key_id") == args.auth_key_id]

    out_lines = []
    for lid in log_ids:
        rec = by_log[lid]
        out_lines.append(render_conversation(lid, rec, meta.get(lid)))
        out_lines.append("")

    text = "\n".join(out_lines)
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
        print(f"已写入 {args.out}（{len(log_ids)} 条对话）", file=sys.stderr)
    else:
        print(text)


if __name__ == "__main__":
    main()
