// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::ffi::CString;
use std::fs::OpenOptions;
use std::io::Write;
use std::sync::atomic::{AtomicU16, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{bail, Context};
use bytes::Bytes;
use nix::libc;
use nix::pty::{forkpty, ForkptyResult, Winsize};
use nix::sys::signal::{kill, Signal};
use nix::sys::wait::{waitpid, WaitPidFlag, WaitStatus};
use nix::unistd::Pid;
use std::collections::BTreeMap;
use std::ffi::OsString;
use std::os::fd::AsRawFd;
use std::os::unix::ffi::OsStringExt;
use tokio::io::unix::AsyncFd;
use tokio::sync::mpsc;

use super::nbio::{read_chunk, set_nonblocking, write_all, PtyFd};
use super::{Backend, BackendInput};
use crate::driver::ExitStatus;

/// Env var that, when set to a writable path, causes `NativePty` to record
/// every PTY master read/write to that file as a hex-encoded trace.
///
/// Format (one record per line, opt-in via env):
///
/// ```text
/// # coop pty trace v1 pid=<n> ts_micros=<unix_us>
/// <micros>\tW\t<hex>
/// <micros>\tR\t<hex>
/// ```
///
/// Existing files are appended to so multiple runs can share the same file
/// when desired; the harness can also use distinct files per scenario.
/// This is a diagnostic facility — see ka-0000002034 for context.
pub const PTY_TRACE_ENV: &str = "COOP_PTY_TRACE_FILE";

/// Native PTY backend that spawns a child process via `forkpty`.
pub struct NativePty {
    master: AsyncFd<PtyFd>,
    child_pid: Pid,
    cols: Arc<AtomicU16>,
    rows: Arc<AtomicU16>,
    reap_interval: Duration,
    tracer: Option<Arc<Mutex<std::fs::File>>>,
}

/// Mark `fd` close-on-exec so processes we fork+exec elsewhere do not inherit it.
// fcntl is a raw libc call; the crate otherwise forbids unsafe.
#[allow(unsafe_code)]
fn set_cloexec(fd: i32) -> anyhow::Result<()> {
    // SAFETY: fcntl on a descriptor we own; F_SETFD only touches the fd flags.
    let rc = unsafe { libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC) };
    if rc == -1 {
        return Err(std::io::Error::last_os_error()).context("fcntl(FD_CLOEXEC) failed");
    }
    Ok(())
}

/// Close every descriptor >= 3 in the post-fork child. Only async-signal-safe
/// calls are used, as required between fork and exec in a multithreaded parent.
// post-fork child cleanup has to be raw libc; see the SAFETY note inside.
#[allow(unsafe_code)]
fn close_inherited_fds() {
    // SAFETY: close_range/close are async-signal-safe; the child owns these
    // copies and is about to exec, so nothing else references them.
    unsafe {
        // close_range is glibc-only in libc; macOS has no equivalent.
        #[cfg(all(target_os = "linux", target_env = "gnu"))]
        if libc::close_range(3, u32::MAX, 0) == 0 {
            return;
        }
        // Kernel < 5.9, or no close_range: fall back to a bounded sweep.
        let max = libc::sysconf(libc::_SC_OPEN_MAX);
        let max = if max <= 0 { 65_536 } else { max.min(65_536) } as i32;
        for fd in 3..max {
            libc::close(fd);
        }
    }
}

/// Path the non-Linux child passes to `execve`.
///
/// Linux execs through `execvpe`, which searches PATH itself. macOS has no
/// `execvpe`, and `execvp` would read PATH from the environment after the
/// fork, which is not async-signal-safe. So the search happens here, in the
/// parent, the way glibc's `execvpe` does it: the caller's PATH, the first
/// executable regular file. A name that is not found is returned as is, so
/// the child's `execve` fails and it exits 127, as it does on Linux.
#[cfg(not(target_os = "linux"))]
fn resolve_program(program: &str) -> anyhow::Result<CString> {
    use std::os::unix::fs::PermissionsExt;
    if !program.contains('/') {
        let path = std::env::var_os("PATH").unwrap_or_else(|| "/usr/bin:/bin".into());
        for dir in std::env::split_paths(&path) {
            let candidate = dir.join(program);
            let executable = std::fs::metadata(&candidate)
                .is_ok_and(|md| md.is_file() && md.permissions().mode() & 0o111 != 0);
            if executable {
                return CString::new(candidate.into_os_string().into_vec())
                    .context("invalid command path");
            }
        }
    }
    CString::new(program).context("invalid command argument")
}

/// Build the child's full environment as NUL-terminated `KEY=VALUE` strings.
///
/// Called in the parent, before `forkpty`: the child cannot safely mutate its
/// environment (see the async-signal-safety note in `spawn`), so the exact
/// envp it should exec with is materialised up front.
fn build_child_env(extra_env: &[(String, String)]) -> anyhow::Result<Vec<CString>> {
    let mut merged: BTreeMap<OsString, OsString> = std::env::vars_os().collect();
    // The Codex auth document goes to $CODEX_HOME/auth.json, never to the agent.
    merged.remove(OsString::from(crate::driver::codex::auth::AUTH_ENV).as_os_str());
    merged.insert("TERM".into(), "xterm-256color".into());
    merged.insert("COOP".into(), "1".into());
    for (key, val) in extra_env {
        merged.insert(key.into(), val.into());
    }
    merged
        .into_iter()
        .map(|(key, val)| {
            let mut entry = key.into_vec();
            entry.push(b'=');
            entry.extend_from_slice(&val.into_vec());
            CString::new(entry).context("environment entry contains an interior NUL")
        })
        .collect()
}

impl NativePty {
    /// Spawn a child process on a new PTY.
    ///
    /// `command` must have at least one element (the program to run).
    /// `extra_env` sets additional environment variables in the child.
    // forkpty requires unsafe: post-fork child is partially initialized
    #[allow(unsafe_code)]
    pub fn spawn(
        command: &[String],
        cols: u16,
        rows: u16,
        extra_env: &[(String, String)],
    ) -> anyhow::Result<Self> {
        let winsize = Winsize { ws_col: cols, ws_row: rows, ws_xpixel: 0, ws_ypixel: 0 };

        if command.is_empty() {
            bail!("spawn requires at least one command element");
        }

        // Everything the child needs is built HERE, before forkpty.
        //
        // fork() clones only the calling thread. Any lock another thread held
        // at that instant stays locked forever in the child, so between fork
        // and exec the child may call only async-signal-safe functions.
        // CString::new, Vec growth and std::env::set_var all allocate or take
        // the global env lock, and doing them post-fork deadlocked the child
        // before it ever reached exec -- the pty_backend tests hung under
        // --test-threads>1 for exactly this reason, with the forked children
        // parked in futex_wait still running the test binary.
        let c_args: Vec<CString> = command
            .iter()
            .map(|s| CString::new(s.as_bytes()))
            .collect::<Result<_, _>>()
            .context("invalid command argument")?;
        let mut argv: Vec<*const libc::c_char> = c_args.iter().map(|a| a.as_ptr()).collect();
        argv.push(std::ptr::null());

        let c_env = build_child_env(extra_env)?;
        let mut envp: Vec<*const libc::c_char> = c_env.iter().map(|e| e.as_ptr()).collect();
        envp.push(std::ptr::null());

        #[cfg(not(target_os = "linux"))]
        let exec_path = resolve_program(&command[0])?;

        // SAFETY: forkpty is unsafe because the child is in a
        // partially-initialized state after fork. We immediately exec.
        let result = unsafe { forkpty(&winsize, None) }.context("forkpty failed")?;

        match result {
            ForkptyResult::Child => {
                // Async-signal-safe calls only, until exec replaces the image.
                // SAFETY: signal/close/execvpe/execve are async-signal-safe; argv and
                // envp point into allocations made before the fork, which this
                // child owns a private copy of and never frees.
                unsafe {
                    // Tokio sets SIGPIPE to SIG_IGN, which the child inherits;
                    // restore it so piped programs behave normally.
                    libc::signal(libc::SIGPIPE, libc::SIG_DFL);

                    // fork() copied every open descriptor of the parent into
                    // this child: other sessions' PTY masters and slaves,
                    // sockets, trace files. Holding another session's pty open
                    // keeps it from ever seeing EOF. The child only needs
                    // 0/1/2, which forkpty already pointed at the slave.
                    close_inherited_fds();

                    #[cfg(target_os = "linux")]
                    libc::execvpe(argv[0], argv.as_ptr(), envp.as_ptr());
                    #[cfg(not(target_os = "linux"))]
                    libc::execve(exec_path.as_ptr(), argv.as_ptr(), envp.as_ptr());

                    // exec failed; _exit avoids running the parent's atexit
                    // handlers or any allocating panic machinery.
                    libc::_exit(127);
                }
            }
            ForkptyResult::Parent { child, master } => {
                // Belt and braces with close_inherited_fds(): children spawned
                // via std::process::Command (which forks + execs without our
                // child-side cleanup) must not inherit this master either.
                set_cloexec(master.as_raw_fd())?;
                set_nonblocking(&master)?;
                let afd = AsyncFd::new(PtyFd(master)).context("AsyncFd::new failed")?;
                let tracer = open_pty_tracer(child.as_raw());
                Ok(Self {
                    master: afd,
                    child_pid: child,
                    cols: Arc::new(AtomicU16::new(cols)),
                    rows: Arc::new(AtomicU16::new(rows)),
                    reap_interval: Duration::from_millis(50),
                    tracer,
                })
            }
        }
    }

    pub fn with_reap_interval(mut self, interval: Duration) -> Self {
        self.reap_interval = interval;
        self
    }
}

/// Open the trace file named by [`PTY_TRACE_ENV`], if set, and write the v1
/// header. On any I/O error the tracer is silently disabled so production
/// PTY behaviour is unaffected by an unwritable path.
fn open_pty_tracer(child_pid: i32) -> Option<Arc<Mutex<std::fs::File>>> {
    let path = std::env::var(PTY_TRACE_ENV).ok()?;
    if path.is_empty() {
        return None;
    }
    let mut file = OpenOptions::new().create(true).append(true).open(&path).ok()?;
    let now_us = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_micros()).unwrap_or(0);
    let header = format!("# coop pty trace v1 pid={child_pid} ts_micros={now_us}\n");
    let _ = file.write_all(header.as_bytes());
    let _ = file.flush();
    Some(Arc::new(Mutex::new(file)))
}

/// Append one trace record. Errors are swallowed: a broken trace file must
/// never crash the session.
fn trace_record(tracer: &Option<Arc<Mutex<std::fs::File>>>, direction: char, bytes: &[u8]) {
    let Some(handle) = tracer else { return };
    let Ok(mut file) = handle.lock() else { return };
    let now_us = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_micros()).unwrap_or(0);
    let mut line = String::with_capacity(32 + bytes.len() * 2);
    line.push_str(&format!("{now_us}\t{direction}\t"));
    for b in bytes {
        line.push_str(&format!("{b:02x}"));
    }
    line.push('\n');
    let _ = file.write_all(line.as_bytes());
    let _ = file.flush();
}

impl Backend for NativePty {
    fn run(
        &mut self,
        output_tx: mpsc::Sender<Bytes>,
        mut input_rx: mpsc::Receiver<BackendInput>,
        mut resize_rx: mpsc::Receiver<(u16, u16)>,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = anyhow::Result<ExitStatus>> + Send + '_>>
    {
        let pid = self.child_pid;
        let tracer = self.tracer.clone();
        Box::pin(async move {
            let mut buf = vec![0u8; 8192];
            let mut input_closed = false;

            loop {
                if input_closed {
                    // Read output + handle resize once input is closed
                    tokio::select! {
                        result = read_chunk(&self.master, &mut buf) => {
                            match result {
                                Ok(0) => break,
                                Ok(n) => {
                                    trace_record(&tracer, 'R', &buf[..n]);
                                    let data = Bytes::copy_from_slice(&buf[..n]);
                                    if output_tx.send(data).await.is_err() {
                                        break;
                                    }
                                }
                                Err(e) if e.raw_os_error() == Some(libc::EIO) => break,
                                Err(e) => return Err(e.into()),
                            }
                        }
                        resize = resize_rx.recv() => {
                            if let Some((cols, rows)) = resize {
                                let _ = self.resize(cols, rows);
                            }
                        }
                    }
                } else {
                    tokio::select! {
                        result = read_chunk(&self.master, &mut buf) => {
                            match result {
                                Ok(0) => break,
                                Ok(n) => {
                                    trace_record(&tracer, 'R', &buf[..n]);
                                    let data = Bytes::copy_from_slice(&buf[..n]);
                                    if output_tx.send(data).await.is_err() {
                                        break;
                                    }
                                }
                                Err(e) if e.raw_os_error() == Some(libc::EIO) => break,
                                Err(e) => return Err(e.into()),
                            }
                        }
                        input = input_rx.recv() => {
                            match input {
                                Some(BackendInput::Write(data)) => {
                                    trace_record(&tracer, 'W', &data);
                                    if let Err(e) = write_all(&self.master, &data).await {
                                        if e.raw_os_error() == Some(libc::EIO) {
                                            break; // Child exited; fall through to wait_for_exit
                                        }
                                        return Err(e.into());
                                    }
                                }
                                Some(BackendInput::Drain(tx)) => {
                                    let _ = tx.send(());
                                }
                                None => input_closed = true,
                            }
                        }
                        resize = resize_rx.recv() => {
                            if let Some((cols, rows)) = resize {
                                let _ = self.resize(cols, rows);
                            }
                        }
                    }
                }
            }

            // Reap child on a blocking thread to avoid blocking the runtime
            let status = tokio::task::spawn_blocking(move || wait_for_exit(pid))
                .await
                .context("join wait thread")??;
            Ok(status)
        })
    }

    fn resize(&self, cols: u16, rows: u16) -> anyhow::Result<()> {
        self.cols.store(cols, Ordering::Relaxed);
        self.rows.store(rows, Ordering::Relaxed);

        let ws =
            rustix::termios::Winsize { ws_col: cols, ws_row: rows, ws_xpixel: 0, ws_ypixel: 0 };
        rustix::termios::tcsetwinsize(self.master.get_ref(), ws)
            .context("TIOCSWINSZ ioctl failed")?;

        Ok(())
    }

    fn child_pid(&self) -> Option<u32> {
        Some(self.child_pid.as_raw() as u32)
    }
}

impl Drop for NativePty {
    fn drop(&mut self) {
        // forkpty places the child in a new session (setsid), so the child PID
        // equals the process group ID. Signal the entire group to clean up
        // grandchildren as well.
        let pgid = Pid::from_raw(-self.child_pid.as_raw());

        // Best-effort graceful shutdown: SIGHUP to the process group.
        let _ = kill(pgid, Signal::SIGHUP);

        // Poll for exit up to 500ms before escalating to SIGKILL.
        let iterations = (500 / self.reap_interval.as_millis().max(1)) as usize;
        for _ in 0..iterations.max(1) {
            match waitpid(self.child_pid, Some(WaitPidFlag::WNOHANG)) {
                Ok(WaitStatus::Exited(..)) | Ok(WaitStatus::Signaled(..)) => return,
                _ => std::thread::sleep(self.reap_interval),
            }
        }

        let _ = kill(pgid, Signal::SIGKILL);
        let _ = waitpid(self.child_pid, Some(WaitPidFlag::WNOHANG));
    }
}

/// Block until the child exits and convert to our `ExitStatus`.
fn wait_for_exit(pid: Pid) -> anyhow::Result<ExitStatus> {
    loop {
        match waitpid(pid, None) {
            Ok(WaitStatus::Exited(_, code)) => {
                return Ok(ExitStatus { code: Some(code), signal: None });
            }
            Ok(WaitStatus::Signaled(_, sig, _)) => {
                return Ok(ExitStatus { code: None, signal: Some(sig as i32) });
            }
            Ok(_) => continue,
            Err(nix::errno::Errno::EINTR) => continue,
            Err(e) => bail!("waitpid failed: {e}"),
        }
    }
}
