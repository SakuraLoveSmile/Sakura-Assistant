#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
mock_hub.py — 契约 api-v1.md 的最小可发 SSE 的测试中枢（仅用于真机验证）。

端点：
  GET  /api/v1/health            → {"ok":true,...}
  GET  /api/v1/version           → {"api":1,"hub":"mock"}
  POST /api/v1/auth/login        → {token,refreshToken,expiresAt,user,serverTime}
  GET  /api/v1/stream?since=N    → SSE 长连接（ping 每 5s；id=<changeSeq>）
  GET  /api/v1/sync?since=N      → {cursor,hasMore:false,changes:[]}
  POST /emit                     → 注入一条 message 事件（见下）
  POST /emit/fault               → 注入一条 fault 事件
  GET  /seq                      → 当前 changeSeq

/emit 请求体（JSON）：
  {
    "title": "新反馈：无法登录",          # 必填
    "body": "反馈正文",
    "kind": "new_message",               # notify.kind: new_message|incident_open|incident_update|incident_resolved
    "faultId": "flt_01TEST",             # incident_* 必填
    "muted": false,
    "sourceId": "src_mock", "sourceName": "MockHub",
    "severity": "warning", "msgKind": "feedback_created"
  }

场景脚本示例（配合 adb reverse tcp:8795 tcp:8795）：
  curl -X POST localhost:8795/emit -H 'Content-Type: application/json' \
    -d '{"title":"测试","kind":"new_message"}'
"""
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

PORT = 8795
TOKEN = "tok_mock_dev"
seq_lock = threading.Lock()
seq = [0]
subscribers = []  # list of (wfile, lock)


def next_seq():
    with seq_lock:
        seq[0] += 1
        return seq[0]


def now_iso():
    return time.strftime("%Y-%m-%dT%H:%M:%S.000Z", time.gmtime())


def sse_send(wfile, event, data, eid=None):
    lock = getattr(wfile, "_wlock", None) or threading.Lock()
    wfile._wlock = lock
    with lock:
        if eid is not None:
            wfile.write(("id: %d\n" % eid).encode())
        wfile.write(("event: %s\n" % event).encode())
        for line in data.split("\n"):
            wfile.write(("data: %s\n" % line).encode())
        wfile.write(b"\n")
        wfile.flush()


def broadcast(event, obj, eid=None):
    data = json.dumps(obj, ensure_ascii=False)
    for w in list(subscribers):
        try:
            sse_send(w, event, data, eid)
        except Exception:
            subscribers.remove(w)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print("[mock_hub]", fmt % args)

    def _auth_ok(self):
        auth = self.headers.get("Authorization", "")
        return auth == ("Bearer " + TOKEN)

    def _json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if u.path == "/api/v1/health":
            return self._json(200, {"ok": True, "version": "mock-0.1.0", "serverTime": now_iso()})
        if u.path == "/api/v1/version":
            return self._json(200, {"api": 1, "hub": "mock-0.1.0"})
        if u.path == "/api/v1/stream":
            if not self._auth_ok():
                return self._json(401, {"error": {"code": "unauthorized", "message": "bad token"}})
            return self._stream(q)
        if u.path == "/api/v1/sync":
            if not self._auth_ok():
                return self._json(401, {"error": {"code": "unauthorized", "message": "bad token"}})
            return self._json(200, {"cursor": seq[0], "hasMore": False, "changes": []})
        if u.path == "/seq":
            return self._json(200, {"seq": seq[0]})
        self._json(404, {"error": {"code": "not_found", "message": u.path}})

    def _stream(self, q):
        since = int(q.get("since", ["0"])[0] or 0)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        self.end_headers()
        w = self.wfile
        subscribers.append(w)
        print("[mock_hub] SSE client connected since=%d" % since)
        try:
            # 若 since 落后超过保留窗口（mock 恒为全窗口），不触发 resync
            while True:
                sse_send(w, "ping", json.dumps({"serverTime": now_iso()}), next_seq())
                time.sleep(5)
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as e:
            print("[mock_hub] stream error:", e)
        finally:
            if w in subscribers:
                subscribers.remove(w)
            print("[mock_hub] SSE client disconnected")

    def do_POST(self):
        u = urlparse(self.path)
        length = int(self.headers.get("Content-Length", "0") or 0)
        body = self.rfile.read(length) if length else b"{}"
        try:
            payload = json.loads(body.decode() or "{}")
        except Exception:
            return self._json(400, {"error": {"code": "invalid_request", "message": "bad json"}})

        if u.path == "/api/v1/auth/login":
            if payload.get("username") == "admin" and payload.get("password") == "admin":
                return self._json(200, {
                    "token": TOKEN,
                    "expiresAt": "2099-01-01T00:00:00.000Z",
                    "refreshToken": "rtok_mock_dev",
                    "user": {"username": "admin"},
                    "serverTime": now_iso(),
                })
            return self._json(401, {"error": {"code": "invalid_credentials", "message": "bad creds"}})

        if u.path == "/emit":
            eid = next_seq()
            kind = payload.get("kind", "new_message")
            fault_id = payload.get("faultId")
            msg = {
                "id": "msg_%06d" % eid,
                "changeSeq": eid,
                "sourceId": payload.get("sourceId", "src_mock"),
                "sourceName": payload.get("sourceName", "MockHub"),
                "kind": payload.get("msgKind", "feedback_created"),
                "severity": payload.get("severity", "info"),
                "title": payload.get("title", "(no title)"),
                "body": payload.get("body", ""),
                "occurredAt": now_iso(),
                "receivedAt": now_iso(),
                "readAt": None,
                "faultId": fault_id,
                "incident": payload.get("incident", 1),
                "notify": {
                    "kind": kind,
                    "faultId": fault_id,
                    "muted": bool(payload.get("muted", False)),
                },
            }
            print("[mock_hub] emit message seq=%d kind=%s title=%s" % (eid, kind, msg["title"]))
            broadcast("message", msg, eid)
            return self._json(200, {"emitted": eid})

        if u.path == "/emit/fault":
            eid = next_seq()
            fault = {
                "id": payload.get("id", "flt_mock"),
                "changeSeq": eid,
                "sourceId": payload.get("sourceId", "src_mock"),
                "sourceName": payload.get("sourceName", "MockHub"),
                "faultKey": payload.get("faultKey", "mock:key"),
                "severity": payload.get("severity", "warning"),
                "title": payload.get("title", "mock fault"),
                "summary": payload.get("summary", ""),
                "state": payload.get("state", "open"),
                "incident": payload.get("incident", 1),
                "openedAt": now_iso(),
                "lastEventAt": now_iso(),
                "resolvedAt": None,
                "eventCount": 1,
                "readAt": None,
                "mutedAt": now_iso() if payload.get("muted") else None,
                "mutedUntil": None,
            }
            print("[mock_hub] emit fault seq=%d id=%s muted=%s" % (eid, fault["id"], bool(payload.get("muted"))))
            broadcast("fault", fault, eid)
            return self._json(200, {"emitted": eid})

        if u.path == "/emit/resync":
            for w in list(subscribers):
                try:
                    sse_send(w, "resync", "{}", None)
                except Exception:
                    pass
            return self._json(200, {"emitted": "resync"})

        self._json(404, {"error": {"code": "not_found", "message": u.path}})


if __name__ == "__main__":
    print("[mock_hub] listening on 0.0.0.0:%d (token=%s)" % (PORT, TOKEN))
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
