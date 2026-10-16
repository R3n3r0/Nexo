#!/usr/bin/env python3
"""Minimal webhook receiver for the event targets of the screenshot demo.

Answers 200 to every request and discards the body. Usage: webhook_sink.py PORT
"""

import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Sink(BaseHTTPRequestHandler):
    def _ok(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_POST = do_GET = do_HEAD = do_PUT = _ok

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Sink).serve_forever()
