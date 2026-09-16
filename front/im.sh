#!/usr/bin/env bash
# 假 IM 适配器 —— 用 multica 的 chat API 当传输层。
# 这是「IM 适配器缝」的本地替身：换掉 BASE/凭据即可换成真飞书。
#
#   ./im.sh new  [title]          新建会话，打印 session id
#   ./im.sh say  <sid> <text>     需求方发言
#   ./im.sh read <sid> [--wait]   读回复（--wait 轮询直到 agent 回合结束）
#   ./im.sh log  <sid>            打印全部消息
set -euo pipefail

BASE="${MULTICA_BASE:-http://localhost:13000}"
WS="${MULTICA_WS:-<workspace-id>}"
AGENT="${DAGUANJIA_ID:-<agent-id>}"
TOK="${MULTICA_TOKEN:-$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.multica/config.json')))['token'])")}"

api() {
  local method="$1" path="$2"; shift 2
  curl -s -X "$method" "$BASE$path" \
    -H "Authorization: Bearer $TOK" \
    -H "X-Workspace-ID: $WS" \
    -H "Content-Type: application/json" "$@"
}

msgs() {
  api GET "/api/chat/sessions/$1/messages?limit=200" \
    | python3 "$(dirname "$0")/im_read.py"
}

case "${1:-}" in
  new)
    api POST /api/chat/sessions \
      -d "{\"agent_id\":\"$AGENT\",\"title\":\"${2:-session}\"}" \
      | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])'
    ;;
  say)
    api POST "/api/chat/sessions/$2/messages" \
      -d "$(python3 -c 'import json,sys;print(json.dumps({"content":sys.argv[1]}))' "$3")" >/dev/null
    echo "sent"
    ;;
  read)
    sid="$2"; wait="$3"
    if [ "$wait" = "--wait" ]; then
      for _ in $(seq 1 90); do
        if [ "$(api GET "/api/chat/sessions/$sid" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status",""))')" != "active" ]; then break; fi
        pending=$(api GET "/api/chat/pending-tasks/has-any" | head -c 20)
        sleep 2
      done
    fi
    msgs "$sid"
    ;;
  log) msgs "$2" ;;
  *) sed -n '2,10p' "$0" ;;
esac
