package k3sauditshipper

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Checkpoint is the durable read position: the inode of the file and the byte
// offset just past the last line whose records the sink accepted. Keying on the
// inode (not the path) is what lets a restart find the right file after the
// apiserver has rotated audit.log to audit-<timestamp>.log.
//
// Inodes are reused after a file is deleted, so the checkpoint also carries a
// fingerprint of the file's first bytes: a different file that landed on a
// recycled inode is detected and read from the start instead of from a stale
// offset in the middle of it.
type Checkpoint struct {
	Inode   uint64 `json:"inode"`
	Offset  int64  `json:"offset"`
	HeadLen int    `json:"headLen"`
	HeadSum string `json:"headSum"`
}

// fingerprintBytes is how much of the file's head the fingerprint covers.
const fingerprintBytes = 256

// headSum fingerprints the first min(n, fingerprintBytes) bytes of f. It returns
// the number of bytes covered and their SHA-256.
func headSum(f *os.File, n int64) (int, string) {
	if n > fingerprintBytes {
		n = fingerprintBytes
	}
	if n <= 0 {
		return 0, ""
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, ""
	}
	sum := sha256.Sum256(buf[:got])
	return got, hex.EncodeToString(sum[:])
}

// matches reports whether f (already known to have cp.Inode) is the file cp was
// taken from: its first HeadLen bytes must hash the same.
func (cp Checkpoint) matches(f *os.File) bool {
	if cp.HeadLen == 0 {
		return true // taken before any data was read; nothing to compare
	}
	n, sum := headSum(f, int64(cp.HeadLen))
	return n == cp.HeadLen && sum == cp.HeadSum
}

// loadCheckpoint reads the checkpoint file. A missing or unreadable file is
// "no checkpoint": the caller falls back to FromStart, so the failure mode of a
// corrupt state file is duplicates, never loss.
func loadCheckpoint(path string) (Checkpoint, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Checkpoint{}, false
	}
	var c Checkpoint
	if err := json.Unmarshal(b, &c); err != nil || c.Inode == 0 {
		return Checkpoint{}, false
	}
	return c, true
}

// saveCheckpoint writes atomically (tmp + rename) so a crash mid-write leaves
// the previous checkpoint, not a torn one.
func saveCheckpoint(path string, c Checkpoint) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// tailer reads complete lines from the audit log, following it across the
// apiserver's rotation (rename audit.log -> audit-<ts>.log, create a new
// audit.log). It never blocks: at EOF next returns errNoData and the caller
// polls.
type tailer struct {
	path     string
	maxLine  int
	f        *os.File
	r        *bufio.Reader
	inode    uint64
	offset   int64    // bytes consumed through the last complete line
	queue    []string // older files still to drain before path, oldest first
	draining bool     // rotation seen: read to EOF once more before switching
}

var errNoData = errors.New("no data yet")

// newTailer positions the tailer. With a checkpoint it resumes in the file that
// owns the inode (looking through rotated siblings) and queues the newer ones;
// without one it starts at the beginning of path when fromStart, else at its end.
func newTailer(path string, maxLine int, cp Checkpoint, haveCP, fromStart bool) (*tailer, error) {
	t := &tailer{path: path, maxLine: maxLine}
	if haveCP {
		rotated := rotatedSiblings(path)
		files := append(append([]string{}, rotated...), path)
		for i, p := range files {
			if !ownsCheckpoint(p, cp) {
				continue
			}
			if err := t.open(p, cp.Offset); err != nil {
				return nil, err
			}
			// Newer rotated files drain next; path itself is reopened last.
			if i < len(rotated) {
				t.queue = rotated[i+1:]
			}
			return t, nil
		}
		// The checkpointed file is gone (its backup aged out): everything left is
		// newer than the checkpoint, so read it all.
		t.queue = rotated
		return t, nil
	}
	if fromStart {
		return t, nil // next() opens path at 0
	}
	if err := t.open(path, -1); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return t, nil
}

// ownsCheckpoint reports whether the file at p is the one cp was taken from.
func ownsCheckpoint(p string, cp Checkpoint) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || inodeOf(fi) != cp.Inode {
		return false
	}
	return cp.matches(f)
}

// rotatedSiblings lists audit-<ts>.log style backups of path, oldest first. The
// apiserver (lumberjack) names them <base>-<timestamp><ext>, and the timestamp
// sorts lexically.
func rotatedSiblings(path string) []string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), base+"-*"+ext))
	sort.Strings(matches)
	return matches
}

// open switches to p at offset (-1 = end of file).
func (t *tailer) open(p string, offset int64) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	switch {
	case offset < 0 || offset > fi.Size():
		// -1 means "tail from the end"; an offset past EOF means the file was
		// truncated or replaced, so start over.
		if offset < 0 {
			offset = fi.Size()
		} else {
			offset = 0
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	t.close()
	t.f, t.r = f, bufio.NewReaderSize(f, 64*1024)
	t.inode, t.offset, t.draining = inodeOf(fi), offset, false
	return nil
}

func (t *tailer) close() {
	if t.f != nil {
		t.f.Close()
		t.f, t.r = nil, nil
	}
}

// position is the checkpoint for the data consumed so far.
func (t *tailer) position() Checkpoint {
	cp := Checkpoint{Inode: t.inode, Offset: t.offset}
	if t.f != nil {
		cp.HeadLen, cp.HeadSum = headSum(t.f, t.offset)
	}
	return cp
}

// next returns the next complete line (without the newline). skipped reports an
// over-long line that was discarded; the position still advances past it.
func (t *tailer) next() (line []byte, skipped bool, err error) {
	for {
		if t.f == nil {
			if err := t.advance(); err != nil {
				return nil, false, err
			}
		}
		line, skipped, err = t.readLine()
		if err == nil {
			return line, skipped, nil
		}
		if !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		// EOF: either wait for more, or move on if the file was rotated away.
		moved, serr := t.rotatedAway()
		if serr != nil {
			return nil, false, serr
		}
		if !moved {
			return nil, false, errNoData
		}
		if !t.draining {
			// First EOF after noticing the rename: appends may have landed between
			// our last read and the rename, so read once more before leaving.
			t.draining = true
			continue
		}
		t.close()
	}
}

// advance opens the next file: queued rotated backups first, then path.
func (t *tailer) advance() error {
	for len(t.queue) > 0 {
		p := t.queue[0]
		t.queue = t.queue[1:]
		if err := t.open(p, 0); err == nil {
			return nil
		}
	}
	if err := t.open(t.path, 0); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNoData // the apiserver has not created it yet
		}
		return err
	}
	return nil
}

// rotatedAway reports whether the open file is no longer what path names (or it
// is a queued backup), i.e. no more data will be appended to it.
func (t *tailer) rotatedAway() (bool, error) {
	cur, err := t.f.Stat()
	if err != nil {
		return false, err
	}
	// Truncation in place (copytruncate-style): start over.
	if cur.Size() < t.offset {
		if _, err := t.f.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
		t.r.Reset(t.f)
		t.offset = 0
		return false, nil
	}
	if len(t.queue) > 0 {
		return true, nil // a backup being drained; the live file comes after
	}
	pfi, err := os.Stat(t.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil // renamed away, replacement not yet created
		}
		return false, err
	}
	return !os.SameFile(cur, pfi), nil
}

// readLine reads one newline-terminated line. A partial line at EOF is not
// consumed: the file is rewound to the line start and io.EOF returned, so a
// record the apiserver is mid-write is read whole on the next poll.
func (t *tailer) readLine() ([]byte, bool, error) {
	var (
		buf     []byte
		tooLong bool
		n       int64
	)
	for {
		chunk, err := t.r.ReadSlice('\n')
		n += int64(len(chunk))
		if !tooLong {
			buf = append(buf, chunk...)
			if len(buf) > t.maxLine {
				tooLong, buf = true, nil
			}
		}
		switch {
		case err == nil:
			t.offset += n
			if tooLong {
				return nil, true, nil
			}
			return buf[:len(buf)-1], false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if _, serr := t.f.Seek(t.offset, io.SeekStart); serr != nil {
				return nil, false, serr
			}
			t.r.Reset(t.f)
			return nil, false, io.EOF
		default:
			return nil, false, fmt.Errorf("read %s: %w", t.path, err)
		}
	}
}
