#!/usr/bin/env python3
"""Serves the design reference. Like `python3 -m http.server`, but nothing is cached, so an edit shows up on reload.

Run: python3 design/reference/serve.py [port]   (default 3000, fails if the port is taken)
"""
import functools
import http.server
import os
import sys


class Handler(http.server.SimpleHTTPRequestHandler):
    def end_headers(self):
        self.send_header("Cache-Control", "no-store")
        super().end_headers()


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 3000
    here = os.path.dirname(os.path.abspath(__file__))
    server = http.server.ThreadingHTTPServer(("0.0.0.0", port), functools.partial(Handler, directory=here))
    print(f"tally prototype on port {port}")
    server.serve_forever()
