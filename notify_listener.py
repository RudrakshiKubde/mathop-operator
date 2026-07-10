# notify_listener.py
#!/usr/bin/env python3
"""
Minimal webhook listener for the mathop-operator's notifyURL callback.

Run it once, in its own terminal:
    python3 notify_listener.py

It listens on port 9091 and pretty-prints the workflow status
(overall phase + task-by-task breakdown) whenever a POST arrives.
Leave it running; you don't need to check anything else.
"""

import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class NotifyHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)

        try:
            status = json.loads(raw)
        except json.JSONDecodeError:
            print("Received a notification but could not parse JSON body.")
            self.send_response(400)
            self.end_headers()
            return

        phase = status.get("phase", "Unknown")
        tasks = status.get("tasks", [])

        print("\n" + "=" * 50)
        if phase == "Succeeded":
            print("WORKFLOW COMPLETE - ALL TASKS SUCCEEDED")
        elif phase == "Failed":
            print("WORKFLOW FAILED")
        else:
            print(f"WORKFLOW NOTIFICATION - phase: {phase}")
        print("=" * 50)

        for t in tasks:
            name = t.get("name", "?")
            task_phase = t.get("phase", "?")
            marker = "OK  " if task_phase == "Succeeded" else "FAIL" if task_phase == "Failed" else "..."
            line = f"[{marker}] {name}: {task_phase}"
            if t.get("error"):
                line += f"  -> {t['error']}"
            print(line)

        print("=" * 50 + "\n")

        # Acknowledge receipt so the controller doesn't retry.
        self.send_response(200)
        self.end_headers()

    def log_message(self, format, *args):
        # Silence the default noisy request logging; we print our own summary above.
        pass


if __name__ == "__main__":
    port = 9091
    server = HTTPServer(("127.0.0.1", port), NotifyHandler)
    print(f"Listening for workflow notifications on http://127.0.0.1:{port}/webhook")
    print("Leave this running. Apply your Workflow with notifyURL set to that address.\n")
    server.serve_forever()