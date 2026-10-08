#!/usr/bin/env python3

import http.server
import sys
import urllib.request


upstream = sys.argv[2]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length < 0 or length > 2 * 1024 * 1024:
            self.send_error(413)
            return
        try:
            request = urllib.request.Request(
                upstream,
                data=self.rfile.read(length),
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(request, timeout=5) as response:
                body = response.read(2 * 1024 * 1024 + 1)
                if len(body) > 2 * 1024 * 1024:
                    self.send_error(502)
                    return
                self.send_response(response.status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
        except Exception:
            self.send_error(502)

    def log_message(self, *_args):
        pass


http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
