#!/bin/bash
# scripts/debug-log-format.sh — 把最近一条 DEBUG REQ/RESP 日志格式化成可读文本
#
# 用法:
#   scripts/debug-log-format.sh              # 输出到 stdout
#   scripts/debug-log-format.sh -o file.txt  # 写入文件
#   scripts/debug-log-format.sh -n 3         # 格式化最近 3 条请求
#   scripts/debug-log-format.sh -f path.log  # 指定日志文件
#
# 依赖: python3
# 日志源: logs/llmio.log (可通过 -f 覆盖)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_FILE="$ROOT/logs/llmio.log"
OUTPUT=""
COUNT=1

while [ $# -gt 0 ]; do
  case "$1" in
    -o|--output) OUTPUT="$2"; shift 2 ;;
    -n|--count)  COUNT="$2"; shift 2 ;;
    -f|--file)   LOG_FILE="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [ ! -f "$LOG_FILE" ]; then
  echo "log file not found: $LOG_FILE" >&2
  exit 1
fi

python3 - "$LOG_FILE" "$COUNT" "$OUTPUT" <<'PY'
import sys, re, json

log_file, count, output = sys.argv[1], int(sys.argv[2]), sys.argv[3] if len(sys.argv)>3 and sys.argv[3] else None

def parse_slog(line):
    m = re.match(r'^(\S+ \S+) INFO \[DEBUG (\w+)\] (.*)$', line)
    if not m: return None
    ts, kind, rest = m.group(1), m.group(2), m.group(3)
    out = {'time': ts, 'kind': kind}
    mm = re.search(r'method=(\S+)', rest)
    if mm: out['method'] = mm.group(1)
    um = re.search(r'url=(\S+)', rest)
    if um: out['url'] = um.group(1)
    sm = re.search(r'status=(\d+)', rest)
    if sm: out['status'] = int(sm.group(1))
    hm = re.search(r'headers="map\[(.*?)\]"', rest)
    if hm:
        h = {}
        for pair in re.finditer(r'(?:(?<=\])|(?<=\s)|^)([\w-]+):([^\s]+)', hm.group(1)):
            h[pair.group(1)] = pair.group(2)
        out['headers'] = h
    bm = re.search(r'body="(.*)"\s*$', rest, re.DOTALL)
    if bm:
        raw = bm.group(1)
        try:
            unesc = raw.replace('\\"', '"').replace('\\\\', '\\')
            out['body'] = json.loads(unesc)
            out['body_ok'] = True
        except Exception as e:
            out['body_ok'] = False
            out['body_raw'] = raw[:2000]
            out['body_err'] = str(e)
    return out

def fmt_req(req, idx):
    L = []
    L.append("=" * 70)
    L.append(f"DEBUG REQUEST #{idx}")
    L.append("=" * 70)
    L.append(f"时间    : {req['time']}")
    L.append(f"方法    : {req.get('method','')}")
    L.append(f"URL     : {req.get('url','')}")
    L.append("")
    L.append(f"Headers ({len(req.get('headers',{}))} 个):")
    for k, v in req.get('headers', {}).items():
        L.append(f"  {k}: {v}")
    L.append("")
    body = req.get('body')
    if req.get('body_ok') and isinstance(body, dict):
        L.append("Body (JSON):")
        L.append(f"  model      : {body.get('model')}")
        L.append(f"  max_tokens : {body.get('max_tokens')}")
        L.append(f"  stream     : {body.get('stream')}")
        msgs = body.get('messages', [])
        L.append(f"  messages   : {len(msgs)} 条")
        for i, msg in enumerate(msgs):
            role = msg.get('role')
            content = msg.get('content')
            if isinstance(content, str):
                preview = content[:200].replace('\n', ' ')
                L.append(f"    [{i}] role={role}")
                L.append(f"         content(str): {preview}...")
            elif isinstance(content, list):
                types = [b.get('type','?') for b in content]
                L.append(f"    [{i}] role={role}")
                L.append(f"         content({len(content)} blocks, types={types})")
                for j, blk in enumerate(content[:2]):
                    t = blk.get('type')
                    if t == 'text':
                        txt = blk.get('text','')[:300].replace('\n',' ')
                        L.append(f"           block[{j}] text: {txt}...")
                    elif t == 'tool_use':
                        L.append(f"           block[{j}] tool_use: name={blk.get('name')} id={blk.get('id')}")
                    elif t == 'tool_result':
                        L.append(f"           block[{j}] tool_result: tool_use_id={blk.get('tool_use_id')}")
                    else:
                        L.append(f"           block[{j}] {t}")
        if body.get('system'):
            sysv = body['system']
            if isinstance(sysv, list):
                L.append(f"  system     : {len(sysv)} 个 block (normalizeRoles 提取)")
                for j, s in enumerate(sysv[:2]):
                    if isinstance(s, dict):
                        txt = s.get('text','')[:150].replace('\n',' ')
                        L.append(f"    system[{j}] type={s.get('type')} text={txt}...")
            else:
                L.append(f"  system     : {str(sysv)[:200]}...")
        other_keys = [k for k in body.keys() if k not in ('model','max_tokens','stream','messages','system')]
        if other_keys:
            L.append(f"  其他字段  : {other_keys}")
    else:
        L.append(f"Body (解析失败: {req.get('body_err','')})")
        L.append(f"  raw 前 2000 字符:")
        L.append(f"  {req.get('body_raw','')}")
    return "\n".join(L)

def fmt_resp(resp, idx):
    L = []
    L.append("=" * 70)
    L.append(f"DEBUG RESPONSE #{idx}")
    L.append("=" * 70)
    L.append(f"时间    : {resp['time']}")
    L.append(f"状态    : {resp.get('status','')}")
    L.append("")
    L.append(f"Headers ({len(resp.get('headers',{}))} 个):")
    for k, v in resp.get('headers', {}).items():
        L.append(f"  {k}: {v}")
    L.append("")
    ct = resp.get('headers',{}).get('Content-Type','')
    if 'text/event-stream' in ct:
        L.append("Body: 流式响应(text/event-stream)")
        L.append("  按设计不捕获 body，避免消费流破坏 SSE 转发给客户端")
    else:
        body = resp.get('body')
        if resp.get('body_ok') and isinstance(body, dict):
            L.append("Body (JSON):")
            L.append(f"  id          : {body.get('id')}")
            L.append(f"  type        : {body.get('type')}")
            L.append(f"  role        : {body.get('role')}")
            L.append(f"  model       : {body.get('model')}")
            L.append(f"  stop_reason : {body.get('stop_reason')}")
            content = body.get('content', [])
            L.append(f"  content     : {len(content)} 个 block")
            for i, blk in enumerate(content[:3]):
                t = blk.get('type')
                if t == 'text':
                    txt = blk.get('text','')[:300].replace('\n',' ')
                    L.append(f"    [{i}] text: {txt}...")
                elif t == 'thinking':
                    txt = blk.get('thinking','')[:300].replace('\n',' ')
                    L.append(f"    [{i}] thinking: {txt}...")
                else:
                    L.append(f"    [{i}] {t}")
            usage = body.get('usage', {})
            L.append(f"  usage       : input={usage.get('input_tokens')} output={usage.get('output_tokens')}")
        else:
            L.append(f"Body: {resp.get('body_raw','')}")
    return "\n".join(L)

reqs = []
resps = []
with open(log_file, 'r', errors='replace') as f:
    for line in f:
        if '[DEBUG REQ]' in line:
            r = parse_slog(line.rstrip('\n'))
            if r: reqs.append(r)
        elif '[DEBUG RESP]' in line:
            r = parse_slog(line.rstrip('\n'))
            if r: resps.append(r)

reqs = reqs[-count:]
resps = resps[-count:]

out_lines = []
for i, req in enumerate(reqs, 1):
    out_lines.append(fmt_req(req, i))
    out_lines.append("")
for i, resp in enumerate(resps, 1):
    out_lines.append(fmt_resp(resp, i))
    out_lines.append("")

content = "\n".join(out_lines)
if output:
    with open(output, 'w', encoding='utf-8') as f:
        f.write(content)
    print(f"written: {output} ({len(content)} bytes)", file=sys.stderr)
else:
    print(content)
PY