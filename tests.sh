#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
xk6 build "${K6_VERSION:-v2.0.0}" --with github.com/phymbert/xk6-sse=.
# Keep CI independent of public SSE services and fail on any incorrect result.
python3 - <<'PYTHON'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
class Handler(BaseHTTPRequestHandler):
    def do_GET(self): self.respond()
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        if body != b'{"ping":true}':
            self.send_error(400)
            return
        self.respond()
    def respond(self):
        self.send_response(200)
        self.send_header('Content-Type','text/event-stream')
        self.end_headers()
        try:
            for i in range(1,5):
                self.wfile.write(f'id: {i}\nevent: message\ndata: {self.command}-{i}\n\n'.encode())
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError): pass
    def log_message(self,*args): pass

import os
from pathlib import Path
import subprocess
import tempfile
import threading

server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
try:
    with tempfile.TemporaryDirectory(prefix='xk6-sse-integration-') as directory:
        script = Path(directory) / 'integration.js'
        script.write_text(r"""import sse from 'k6/x/sse';
import {check} from 'k6';
export const options = {vus:5,iterations:20,thresholds:{checks:['rate==1'],sse_event:['count==120']}};
export default function () {
 for (const method of ['GET','POST']) {
  let count=0, errors=0, content=true, closed=false;
  const response=sse.open(__ENV.SSE_TEST_URL,{method,body:method==='POST'?'\u007b"ping":true\u007d':undefined}, client=>{
   client.on('event',event=>{count++;content=content && event.data===`${method}-${count}`;if(count===3){client.close();closed=true;}});
   client.on('error',()=>{errors++;});
  });
  check(response,{'status 200':r=>r.status===200,'three events':()=>count===3,'correct data':()=>content,'closed':()=>closed,'no errors':()=>errors===0});
 }
}
""")
        env = dict(os.environ, SSE_TEST_URL=f'http://127.0.0.1:{server.server_port}/')
        result = subprocess.run(['./k6', 'run', str(script)], env=env, timeout=60)
finally:
    server.shutdown()
    server.server_close()
    thread.join()
raise SystemExit(result.returncode)
PYTHON
