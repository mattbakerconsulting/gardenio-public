// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::io::Write;

use super::LogWatcher;

#[test]
fn reads_new_lines_from_file() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let path = dir.path().join("test.jsonl");
    std::fs::write(&path, "{\"type\":\"system\"}\n{\"type\":\"assistant\"}\n")?;

    let mut watcher = LogWatcher::new(path);
    let lines = watcher.read_new_lines()?;
    assert_eq!(lines.len(), 2);
    assert_eq!(lines[0], r#"{"type":"system"}"#);
    assert_eq!(lines[1], r#"{"type":"assistant"}"#);
    Ok(())
}

#[test]
fn returns_empty_when_file_unchanged() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let path = dir.path().join("test.jsonl");
    std::fs::write(&path, "{\"line\":1}\n")?;

    let mut watcher = LogWatcher::new(path);
    let _ = watcher.read_new_lines()?;

    // Second read with no changes
    let lines = watcher.read_new_lines()?;
    assert!(lines.is_empty());
    Ok(())
}

#[test]
fn handles_nonexistent_file() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let path = dir.path().join("missing.jsonl");

    let mut watcher = LogWatcher::new(path);
    let lines = watcher.read_new_lines()?;
    assert!(lines.is_empty());
    Ok(())
}

#[test]
fn reports_correct_offset() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let path = dir.path().join("test.jsonl");
    let content = "{\"a\":1}\n";
    std::fs::write(&path, content)?;

    let mut watcher = LogWatcher::new(path.clone());
    assert_eq!(watcher.offset(), 0);

    let _ = watcher.read_new_lines()?;
    assert_eq!(watcher.offset(), content.len() as u64);

    // Append more data
    let mut file = std::fs::OpenOptions::new().append(true).open(&path)?;
    write!(file, "{{\"b\":2}}\n")?;
    drop(file);

    let lines = watcher.read_new_lines()?;
    assert_eq!(lines.len(), 1);
    assert_eq!(lines[0], r#"{"b":2}"#);
    Ok(())
}

#[cfg(unix)]
#[test]
fn unchanged_file_is_not_reopened() -> anyhow::Result<()> {
    // Regression for the coop idle-CPU busy loop (gd-hMkeX9MkIT): `notify`'s
    // default inotify mask includes IN_OPEN, so an implementation that always
    // opens the file to check for new data generates its own watch event on
    // every check, which re-wakes the watcher and spins forever. Assert we
    // never actually open the file when its length hasn't changed: strip
    // read permission after the first read, then confirm a second read still
    // succeeds (from the length comparison alone) instead of failing with a
    // permission error from a real `File::open`.
    use std::os::unix::fs::PermissionsExt;

    let dir = tempfile::tempdir()?;
    let path = dir.path().join("test.jsonl");
    std::fs::write(&path, "{\"line\":1}\n")?;

    let mut watcher = LogWatcher::new(path.clone());
    let _ = watcher.read_new_lines()?;

    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o000))?;
    let result = watcher.read_new_lines();
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644))?;

    assert!(result?.is_empty());
    Ok(())
}

#[test]
fn handles_file_truncation() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let path = dir.path().join("test.jsonl");

    // Write initial content and read past it.
    std::fs::write(&path, "{\"a\":1}\n{\"b\":2}\n{\"c\":3}\n")?;
    let mut watcher = LogWatcher::new(path.clone());
    let lines = watcher.read_new_lines()?;
    assert_eq!(lines.len(), 3);
    let old_offset = watcher.offset();
    assert!(old_offset > 0);

    // Truncate the file (simulates `/clear` rewriting the session log).
    std::fs::write(&path, "{\"new\":1}\n")?;

    // Watcher should detect truncation, reset offset, and read the new content.
    let lines = watcher.read_new_lines()?;
    assert_eq!(lines.len(), 1);
    assert_eq!(lines[0], r#"{"new":1}"#);
    assert!(watcher.offset() < old_offset);
    Ok(())
}

/// CPU time the calling thread has used so far. schedstat (ns) is absent on
/// kernels built without CONFIG_SCHED_INFO, some CI runners among them, so
/// fall back to utime+stime from stat, which /proc reports in USER_HZ (100)
/// ticks on every Linux.
#[cfg(target_os = "linux")]
fn thread_cpu_time() -> Option<std::time::Duration> {
    let sched = std::fs::read_to_string("/proc/thread-self/schedstat").unwrap_or_default();
    if let Some(ns) = sched.split_whitespace().next().and_then(|v| v.parse().ok()) {
        return Some(std::time::Duration::from_nanos(ns));
    }
    let stat = std::fs::read_to_string("/proc/thread-self/stat").ok()?;
    // Fields after the parenthesised comm: state is field 3, utime 14, stime 15.
    let mut rest = stat.rsplit_once(')')?.1.split_whitespace().skip(11);
    let utime: u64 = rest.next()?.parse().ok()?;
    let stime: u64 = rest.next()?.parse().ok()?;
    Some(std::time::Duration::from_millis((utime + stime) * 10))
}

#[cfg(target_os = "linux")]
#[tokio::test(flavor = "current_thread")]
async fn missing_parent_dir_does_not_spin() -> anyhow::Result<()> {
    // Regression: a fresh Claude creates its project dir after coop binds the
    // transcript path, so the parent watch fails. That dropped the only wake
    // sender, `recv()` returned `None` at once on every turn, and the loop
    // spun on stat() at 30-90% of a core for the session's life. On a
    // current-thread runtime a spin shows up as this thread's CPU time.
    let dir = tempfile::tempdir()?;
    let path = dir.path().join("not-yet").join("session.jsonl");
    let watcher = LogWatcher::new(path).with_poll_interval(std::time::Duration::from_secs(60));
    let (line_tx, _line_rx) = tokio::sync::mpsc::channel(8);
    let shutdown = tokio_util::sync::CancellationToken::new();
    let task = tokio::spawn(watcher.run(line_tx, shutdown.clone()));

    let before = thread_cpu_time()
        .ok_or_else(|| anyhow::anyhow!("/proc/thread-self schedstat and stat both unreadable"))?;
    tokio::time::sleep(std::time::Duration::from_millis(500)).await;
    let used =
        thread_cpu_time().ok_or_else(|| anyhow::anyhow!("thread CPU time unreadable"))? - before;
    shutdown.cancel();
    task.await?;

    assert!(
        used < std::time::Duration::from_millis(100),
        "watch loop used {used:?} of CPU in 500ms"
    );
    Ok(())
}

#[tokio::test]
async fn rearms_watch_once_parent_dir_appears() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let parent = dir.path().join("not-yet");
    let path = parent.join("session.jsonl");
    let watcher =
        LogWatcher::new(path.clone()).with_poll_interval(std::time::Duration::from_millis(50));
    let (line_tx, mut line_rx) = tokio::sync::mpsc::channel(8);
    let shutdown = tokio_util::sync::CancellationToken::new();
    let task = tokio::spawn(watcher.run(line_tx, shutdown.clone()));

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    std::fs::create_dir(&parent)?;
    std::fs::write(&path, "{\"a\":1}\n")?;

    let lines = tokio::time::timeout(std::time::Duration::from_secs(5), line_rx.recv()).await?;
    assert_eq!(lines, Some(vec![r#"{"a":1}"#.to_owned()]));
    shutdown.cancel();
    task.await?;
    Ok(())
}
