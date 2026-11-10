#!/bin/sh
# A plain request, then the same one streamed. The API key can be any text.
URL=${WEND_URL:-http://localhost:8888}

curl -s "$URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model": "auto", "messages": [{"role": "user", "content": "Name three rivers in one line."}]}'
echo

# "stream": true sends the answer as it is written, as server-sent events
curl -sN "$URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model": "fast", "stream": true, "messages": [{"role": "user", "content": "Count from 1 to 5."}]}'
