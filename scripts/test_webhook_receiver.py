import hashlib
import hmac
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer


secret = os.environ["SETTLEKIT_TEST_WEBHOOK_SECRET"].encode()
output_path = sys.argv[2]


class Receiver(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        if length < 1 or length > 4096:
            self.send_error(413)
            return
        body = self.rfile.read(length)
        event_id = self.headers.get("X-SettleKit-Event-Id", "")
        try:
            timestamp = int(self.headers.get("X-SettleKit-Timestamp", ""))
        except ValueError:
            self.send_error(401)
            return
        signed = str(timestamp).encode() + b"." + event_id.encode() + b"." + body
        expected = "v1=" + hmac.new(secret, signed, hashlib.sha256).hexdigest()
        if abs(time.time() - timestamp) > 300 or not hmac.compare_digest(expected, self.headers.get("X-SettleKit-Signature", "")):
            self.send_error(401)
            return
        event = json.loads(body)
        with open(output_path, "a", encoding="utf-8") as output:
            output.write(json.dumps(event, separators=(",", ":")) + "\n")
        self.send_response(204)
        self.end_headers()

    def log_message(self, *_args):
        pass


HTTPServer(("127.0.0.1", int(sys.argv[1])), Receiver).serve_forever()
