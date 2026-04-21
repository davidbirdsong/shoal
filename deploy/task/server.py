import http.server
import os

port = int(os.environ.get("SHOAL_PORT", 8000))


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = f"hello from {os.uname().nodename}\n".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass  # suppress access log noise


http.server.HTTPServer(("", port), Handler).serve_forever()
