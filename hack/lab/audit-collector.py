"""A minimal append-only audit collector for the lab: every POST body (one JSON audit event) is appended as one line to a file.
It requires a bearer token and answers 503 to the first request on purpose, so the sender's retry is exercised.
usage: python audit-collector.py PORT TOKEN OUTFILE   (listens on 127.0.0.1 only)"""
import http.server
import json
import sys

port, token, out = int(sys.argv[1]), sys.argv[2], sys.argv[3]
state = {"n": 0}


class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        state["n"] += 1
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if self.headers.get("Authorization") != "Bearer " + token:
            self.send_response(401)
            self.end_headers()
            return
        if state["n"] == 1:
            self.send_response(503)  # the first delivery fails: the sender must retry
            self.end_headers()
            return
        try:
            json.loads(body)
        except ValueError:
            self.send_response(400)
            self.end_headers()
            return
        with open(out, "ab") as f:
            f.write(body.rstrip(b"\n") + b"\n")
        self.send_response(204)
        self.end_headers()

    def log_message(self, *a):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
