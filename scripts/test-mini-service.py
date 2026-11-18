#!/usr/bin/env python3
"""Check that loopback acceptance services can start without hostname resolution."""
import importlib.util
import os
from pathlib import Path
from unittest.mock import patch

source = Path(__file__).resolve().parents[1] / "examples/mini/service.py"
spec = importlib.util.spec_from_file_location("mini_service", source)
service = importlib.util.module_from_spec(spec)
spec.loader.exec_module(service)
with patch.dict(os.environ, RDR_HOST="127.0.0.1", RDR_PORT="0"):
    with patch("socket.getfqdn", side_effect=AssertionError("Loopback fixture must not resolve hostnames")):
        with patch.object(service.ThreadingHTTPServer, "serve_forever", lambda server: server.server_close()):
            service.serve("catalog")
print("PASS: loopback service starts without DNS")
