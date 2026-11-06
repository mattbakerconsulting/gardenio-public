#!/usr/bin/env python3
"""Minimal ACP (Agent Client Protocol) client for the Gemini phase-0 spike.
Drives `gemini --acp` over stdio JSON-RPC and records every frame to a JSONL file.
usage: acp_client.py <gemini-cmd...> -- <scenario> <out.jsonl>
scenarios: basic | tool | cancel | resume
"""
import json, subprocess, sys, threading, time, os, queue

sep = sys.argv.index('--')
cmd = sys.argv[1:sep]; scenario = sys.argv[sep+1]; out = open(sys.argv[sep+2], 'w')
p = subprocess.Popen(cmd + ['--acp'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, cwd=os.getcwd())
q = queue.Queue(); nid = [0]
def rec(direction, obj): out.write(json.dumps({'dir': direction, 'frame': obj}) + '\n'); out.flush()
def reader():
    for l in p.stdout:
        l = l.strip()
        if not l: continue
        try: o = json.loads(l)
        except Exception: rec('raw', l); continue
        rec('<-', o); q.put(o)
threading.Thread(target=reader, daemon=True).start()
errlog = []
threading.Thread(target=lambda: [errlog.append(l) for l in p.stderr], daemon=True).start()
def send(method, params=None, notify=False):
    o = {'jsonrpc': '2.0', 'method': method}
    if params is not None: o['params'] = params
    if not notify:
        nid[0] += 1; o['id'] = nid[0]
    rec('->', o); p.stdin.write(json.dumps(o) + '\n'); p.stdin.flush(); return o.get('id')
def wait(id_, timeout=60, on_req=None):
    end = time.time() + timeout
    while time.time() < end:
        try: o = q.get(timeout=1)
        except queue.Empty: continue
        if o.get('id') == id_ and ('result' in o or 'error' in o): return o
        if 'method' in o and 'id' in o:  # server->client request (permission, fs)
            res = on_req(o) if on_req else {'outcome': {'outcome': 'cancelled'}}
            rec('->', {'jsonrpc': '2.0', 'id': o['id'], 'result': res}); p.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': o['id'], 'result': res}) + '\n'); p.stdin.flush()
    return None
def allow(o):
    opts = (o.get('params') or {}).get('options') or []
    pick = next((x for x in opts if 'allow' in json.dumps(x).lower()), opts[0] if opts else None)
    return {'outcome': {'outcome': 'selected', 'optionId': pick['optionId']}} if pick else {'outcome': {'outcome': 'cancelled'}}

r = wait(send('initialize', {'protocolVersion': 1, 'clientCapabilities': {'fs': {'readTextFile': False, 'writeTextFile': False}}}))
auth = (r or {}).get('result', {}).get('authMethods', [])
mid = next((a['id'] for a in auth if 'api' in a['id'].lower()), auth[0]['id'] if auth else 'gemini-api-key')
wait(send('authenticate', {'methodId': mid}))
sid = os.environ.get('ACP_SESSION')
if scenario == 'resume' and sid:
    wait(send('session/load', {'sessionId': sid, 'cwd': os.getcwd(), 'mcpServers': []}))
else:
    r = wait(send('session/new', {'cwd': os.getcwd(), 'mcpServers': []})); sid = r['result']['sessionId']
text = {'basic': 'say pong', 'tool': os.environ.get('ACP_TOOLCMD','MOCK:TOOL echo hi'), 'cancel': 'MOCK:SLOW', 'resume': 'again', 'queue': 'MOCK:SLOW'}[scenario]
pid = send('session/prompt', {'sessionId': sid, 'prompt': [{'type': 'text', 'text': text}]})
if scenario == 'cancel':
    time.sleep(3); send('session/cancel', {'sessionId': sid}, notify=True)
if scenario == 'queue':
    time.sleep(3); pid2 = send('session/prompt', {'sessionId': sid, 'prompt': [{'type': 'text', 'text': 'second prompt'}]})
    time.sleep(3); send('session/cancel', {'sessionId': sid}, notify=True)
    r2 = wait(pid2, 30, allow); rec('result2', r2)
r = wait(pid, 60, allow)
rec('result', r)
send('shutdown') if False else None
p.stdin.close(); time.sleep(0.5); p.terminate()
open(sys.argv[sep+2] + '.stderr', 'w').write(''.join(errlog))
print('done', scenario, 'sid', sid, 'stopReason', ((r or {}).get('result') or {}).get('stopReason'))
