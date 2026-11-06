import os, pty, sys, time, select, re, fcntl, termios, struct
out, steps = sys.argv[1], sys.argv[2:]   # steps: "wait:N" or "send:text" (\r for enter)
pid, fd = pty.fork()
if pid == 0:
    os.environ['TERM']='xterm-256color'
    os.execvp('node', ['node', os.environ['GBIN'], *os.environ.get('GARGS','').split()])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
buf = b''
def pump(t):
    global buf
    end = time.time()+t
    while time.time() < end:
        r,_,_ = select.select([fd],[],[],0.2)
        if r:
            try: d = os.read(fd, 65536)
            except OSError: return
            if not d: return
            buf += d
for s in steps:
    k, v = s.split(':',1)
    if k=='wait': pump(float(v))
    else: os.write(fd, v.encode().decode('unicode_escape').encode()); pump(0.3)
try: os.kill(pid, 15)
except OSError: pass
time.sleep(0.5)
open(out,'wb').write(buf)
txt = re.sub(rb'\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07', b'', buf).decode(errors='replace')
open(out+'.txt','w').write(txt)
print(len(buf),'bytes')
