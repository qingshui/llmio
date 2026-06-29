#!/bin/bash
# scripts/debug-log-format.sh — 格式化最近一条 DEBUG 日志（REQ/RESP/STREAM）
#
# 用法:
#   scripts/debug-log-format.sh              # 输出到 stdout
#   scripts/debug-log-format.sh -o file.txt  # 写入文件
#   scripts/debug-log-format.sh -n 3         # 格式化最近 3 组
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
    m = re.match(r'^(\S+ \S+) INFO \[DEBUG ([\w ]+)\] (.*)$', line)
    if not m: return None
    ts, kind, rest = m.group(1), m.group(2), m.group(3)
    out = {'time': ts, 'kind': kind, 'raw': rest}
    mm = re.search(r'method=(\S+)', rest)
    if mm: out['method'] = mm.group(1)
    um = re.search(r'url=(\S+)', rest)
    if um: out['url'] = um.group(1)
    sm = re.search(r'status=(\d+)', rest)
    if sm: out['status'] = int(sm.group(1))
    lm = re.search(r'logId=(\d+)', rest)
    if lm: out['logId'] = int(lm.group(1))
    cm = re.search(r'chunk=(\d+)', rest)
    if cm: out['chunk'] = int(cm.group(1))
    em = re.search(r'elapsed_ms=(\d+)', rest)
    if em: out['elapsed_ms'] = int(em.group(1))
    bm2 = re.search(r'bytes=(\d+)', rest)
    if bm2: out['bytes'] = int(bm2.group(1))
    chm = re.search(r'chunks=(\d+)', rest)
    if chm: out['chunks'] = int(chm.group(1))
    tm = re.search(r'total_ms=(\d+)', rest)
    if tm: out['total_ms'] = int(tm.group(1))
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
            out['body_raw'] = raw
            out['body_err'] = str(e)
    evm = re.search(r'event="(.*)"\s*(?:elapsed_ms=\d+)?\s*$', rest, re.DOTALL)
    if evm:
        try:
            unesc = evm.group(1).replace('\\"', '"').replace('\\\\', '\\')
            out['event'] = unesc
        except:
            out['event'] = evm.group(1)
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
                L.append(f"    [{i}] role={role}")
                L.append(f"         content(str): {content}")
            elif isinstance(content, list):
                types = [b.get('type','?') for b in content]
                L.append(f"    [{i}] role={role}")
                L.append(f"         content({len(content)} blocks, types={types})")
                for j, blk in enumerate(content):
                    t = blk.get('type')
                    if t == 'text':
                        L.append(f"           block[{j}] text: {blk.get('text','')}")
                    elif t == 'tool_use':
                        L.append(f"           block[{j}] tool_use: name={blk.get('name')} id={blk.get('id')}")
                        if blk.get('input'):
                            L.append(f"             input: {json.dumps(blk.get('input'), ensure_ascii=False)}")
                    elif t == 'tool_result':
                        L.append(f"           block[{j}] tool_result: tool_use_id={blk.get('tool_use_id')}")
                        if blk.get('content'):
                            L.append(f"             content: {blk.get('content')}")
                    else:
                        L.append(f"           block[{j}] {t}: {json.dumps(blk, ensure_ascii=False)}")
        if body.get('system'):
            sysv = body['system']
            if isinstance(sysv, list):
                L.append(f"  system     : {len(sysv)} 个 block (normalizeRoles 提取)")
                for j, s in enumerate(sysv):
                    if isinstance(s, dict):
                        L.append(f"    system[{j}] type={s.get('type')} text={s.get('text','')}")
            else:
                L.append(f"  system     : {sysv}")
        other_keys = [k for k in body.keys() if k not in ('model','max_tokens','stream','messages','system')]
        if other_keys:
            L.append(f"  其他字段  : {other_keys}")
            for k in other_keys:
                v = body.get(k)
                if isinstance(v, (dict, list)):
                    L.append(f"    {k}: {json.dumps(v, ensure_ascii=False)}")
                else:
                    L.append(f"    {k}: {v}")
    else:
        L.append(f"Body (解析失败: {req.get('body_err','')})")
        L.append(f"  raw:")
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
        L.append("  body 不在 [DEBUG RESP] 中捕获，见 [DEBUG STREAM] 分块日志")
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
            for i, blk in enumerate(content):
                t = blk.get('type')
                if t == 'text':
                    L.append(f"    [{i}] text: {blk.get('text','')}")
                elif t == 'thinking':
                    L.append(f"    [{i}] thinking: {blk.get('thinking','')}")
                else:
                    L.append(f"    [{i}] {t}: {json.dumps(blk, ensure_ascii=False)}")
            usage = body.get('usage', {})
            L.append(f"  usage       : input={usage.get('input_tokens')} output={usage.get('output_tokens')}")
        else:
            L.append(f"Body: {resp.get('body_raw','')}")
    return "\n".join(L)

def parse_stream_event(event_str):
    # slog 把真换行转义成字面 \n，先反转义
    event_str = event_str.replace("\\n", "\n")
    """解析 SSE event 字符串，返回 (event_type, data_json 或 raw)"""
    event_type = ''
    data_str = ''
    for line in event_str.split('\n'):
        if line.startswith('event: '):
            event_type = line[len('event: '):].strip()
        elif line.startswith('data: '):
            data_str += line[len('data: '):]
    if data_str:
        try:
            return event_type, json.loads(data_str)
        except:
            return event_type, data_str
    return event_type, None

def fmt_stream_group(logId, chunks, end_info, idx):
    L = []
    L.append("=" * 70)
    L.append(f"DEBUG STREAM #{idx}  (logId={logId})")
    L.append("=" * 70)
    if end_info:
        L.append(f"总耗时  : {end_info.get('total_ms','')} ms")
        L.append(f"总 chunk: {end_info.get('chunks','')}")
    else:
        L.append(f"(无 END 标记，共 {len(chunks)} chunk)")
    L.append("")
    L.append(f"Chunks ({len(chunks)} 个):")
    for c in chunks:
        ev = c.get('event', '')
        ev_type, ev_data = parse_stream_event(ev)
        L.append(f"  [{c.get('chunk','?')}] elapsed={c.get('elapsed_ms','?')}ms bytes={c.get('bytes','?')} type={ev_type}")
        if isinstance(ev_data, dict):
            keys = list(ev_data.keys())
            L.append(f"       keys: {keys}")
            for k in ('type','delta','content_block','message','usage','index','stop_reason'):
                if k in ev_data:
                    v = ev_data[k]
                    if isinstance(v, (dict, list)):
                        L.append(f"       {k}: {json.dumps(v, ensure_ascii=False)}")
                    else:
                        L.append(f"       {k}: {v}")
        elif ev_data:
            L.append(f"       data: {ev_data}")
        L.append(f"       raw: {ev}")
    return "\n".join(L)

# 收集所有 DEBUG 日志行
reqs = []
resps = []
streams = {}  # logId -> {'chunks': [], 'end': None}

with open(log_file, 'r', errors='replace') as f:
    for line in f:
        line = line.rstrip('\n')
        if '[DEBUG REQ]' in line:
            r = parse_slog(line)
            if r: reqs.append(r)
        elif '[DEBUG RESP]' in line:
            r = parse_slog(line)
            if r: resps.append(r)
        elif '[DEBUG STREAM END]' in line:
            r = parse_slog(line)
            if r:
                lid = r.get('logId', 0)
                if lid not in streams:
                    streams[lid] = {'chunks': [], 'end': None}
                streams[lid]['end'] = r
        elif '[DEBUG STREAM]' in line:
            r = parse_slog(line)
            if r:
                lid = r.get('logId', 0)
                if lid not in streams:
                    streams[lid] = {'chunks': [], 'end': None}
                streams[lid]['chunks'].append(r)

# 取最近的 count 个
reqs = reqs[-count:]
resps = resps[-count:]
stream_items = list(streams.items())[-count:]

out_lines = []
for i, req in enumerate(reqs, 1):
    out_lines.append(fmt_req(req, i))
    out_lines.append("")
for i, resp in enumerate(resps, 1):
    out_lines.append(fmt_resp(resp, i))
    out_lines.append("")
for i, (lid, info) in enumerate(stream_items, 1):
    out_lines.append(fmt_stream_group(lid, info['chunks'], info['end'], i))
    out_lines.append("")

content = "\n".join(out_lines)
if output:
    with open(output, 'w', encoding='utf-8') as f:
        f.write(content)
    print(f"written: {output} ({len(content)} bytes)", file=sys.stderr)
else:
    print(content)
PY