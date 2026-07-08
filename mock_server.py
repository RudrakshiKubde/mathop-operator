# mock_server.py
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json

class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = json.loads(self.rfile.read(length) or b'{}')

        if self.path == '/add':
            result = {"sum": body.get("x", 0) + body.get("y", 0)}
        elif self.path == '/square':
            v = body.get("value", 0)
            result = {"square": v * v}
        else:
            self.send_response(404)
            self.end_headers()
            return

        payload = json.dumps(result).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, format, *args):
        print(f"[mock_server] {self.path} <- {args}")

from http.server import ThreadingHTTPServer
ThreadingHTTPServer(("0.0.0.0", 9090), Handler).serve_forever()