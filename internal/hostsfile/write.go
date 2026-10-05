package hostsfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/manprint/docker-hoster-injector/internal/config"
)

// DefaultPerm is the mode of a hosts file created by this agent. 0644 keeps it
// world readable, which the resolver requires.
const DefaultPerm fs.FileMode = 0o644

// DirPerm is used when the agent has to create the hosts file itself, which
// happens in tests and in the "dir" mount mode on a fresh volume.
const DirPerm fs.FileMode = 0o755

// TempPrefix starts the name of the temporary file written in "dir" mode before
// it is renamed over the hosts file. A process killed between creating it and
// the rename leaves it behind, which is why CleanStale knows the prefix.
const TempPrefix = ".hosts-docker-hoster-injector-"

// staleTempAge is how old a leftover temporary file must be before it is
// removed. A younger one may belong to a second agent that is writing right
// now.
const staleTempAge = time.Minute

// ErrNotManaged is returned when the file is not under this agent's control.
var ErrNotManaged = errors.New("hosts file does not contain a managed block")

// Writer owns a hosts file and rewrites only its own block.
//
// It is safe for concurrent use: all mutations serialise on an in-process
// mutex and, in file mode, on an advisory flock that also serialises against a
// second instance of the agent.
type Writer struct {
	path string
	mode config.MountMode

	// suffix, when set, is the DNS suffix every record of ours ends with. It
	// is how recovery tells a stale record of ours from a line of the
	// operator's in a block left open by a crash.
	suffix string

	mu sync.Mutex

	// lastGood is the most recent text known to be complete and parseable.
	// It is the content restored when the file on disk turns out to be
	// damaged, so a crash in the middle of a write is recoverable.
	lastGood []byte
	// lastApplied is the exact text of the last successful write, used to
	// detect that nothing changed and skip the write entirely.
	lastApplied []byte
	// writes counts successful writes, for the metrics endpoint.
	writes uint64
}

// NewWriter returns a Writer for the given hosts file.
func NewWriter(path string, mode config.MountMode) (*Writer, error) {
	if path == "" {
		return nil, errors.New("hosts file path must not be empty")
	}
	return &Writer{path: path, mode: mode}, nil
}

// SetOwnedSuffix tells the writer which DNS suffix its own records carry.
//
// It matters only when recovering a block that has no end marker: everything
// after the opening marker is then of unknown origin, and without the suffix
// the writer can only assume that every address line in it is its own. With
// it, a line such as "192.168.1.10 nas.home" is recognised as the operator's
// and kept.
func (w *Writer) SetOwnedSuffix(suffix string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.suffix = strings.ToLower(strings.Trim(strings.TrimSpace(suffix), "."))
}

// Path is the file this writer manages.
func (w *Writer) Path() string { return w.path }

// Mode reports the write strategy in use. A restart needs it to rebuild the
// writer from configuration alone.
func (w *Writer) Mode() config.MountMode { return w.mode }

// Writes returns the number of successful writes performed.
func (w *Writer) Writes() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// Snapshot returns the last text successfully applied, or the text found on
// disk at startup. It never returns a partially written buffer.
func (w *Writer) Snapshot() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastApplied != nil {
		return bytes.Clone(w.lastApplied)
	}
	return bytes.Clone(w.lastGood)
}

// Adopt reads the current file and remembers it as the recovery baseline.
// It is called once at startup so that a damaged file left behind by a crash
// is detected before anything is written over it.
func (w *Writer) Adopt() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.adoptLocked()
}

func (w *Writer) adoptLocked() error {
	data, err := os.ReadFile(w.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Nothing to adopt. An empty baseline is still valid: the first
		// write creates the file with just the managed block.
		w.lastGood = nil
		return nil
	case err != nil:
		return fmt.Errorf("read the hosts file: %w", err)
	}

	if _, perr := Parse(bytes.NewReader(data)); perr != nil {
		return fmt.Errorf("%s is not readable as a hosts file: %w", w.path, perr)
	}
	w.lastGood = bytes.Clone(data)
	w.lastApplied = bytes.Clone(data)
	return nil
}

// Apply writes the desired entries, replacing the managed block and leaving
// every other byte of the file untouched.
//
// It is idempotent: when the rendered block already matches what is on disk,
// nothing is written and no file modification time changes.
func (w *Writer) Apply(entries []Entry) (changed bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Re-read under the lock. Between reconciles the operator, NetworkManager
	// or systemd may have edited the file, and overwriting their change from
	// a stale in-memory copy is exactly the bug this avoids.
	if err := w.adoptLocked(); err != nil {
		return false, err
	}

	current, err := os.ReadFile(w.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		current = nil
	case err != nil:
		return false, fmt.Errorf("read the hosts file: %w", err)
	}

	f, err := Parse(bytes.NewReader(current))
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", w.path, err)
	}

	// A block left open by a crash must not be trusted or extended blindly:
	// it is rebuilt from scratch instead. What followed the opening marker is
	// ours only if it looks like ours; anything else is the operator's.
	if f.BlockTruncated {
		f.Lines = append(f.Lines, w.salvageOrphans(f.Orphans)...)
		f.Managed = nil
		f.Orphans = nil
		f.HadBlock = false
	}

	// A block with nothing in it is not worth keeping: Render leaves it out, so
	// a file that has one is not up to date when no entries are wanted.
	emptyBlockLeft := f.HadBlock && len(GroupByAddr(entries)) == 0
	if SameManagedBlock(f, entries) && !f.BlockTruncated && !emptyBlockLeft {
		// Nothing to do. lastApplied already reflects the file.
		return false, nil
	}

	next := Render(f, entries)
	if err := w.writeLocked(next); err != nil {
		return false, err
	}

	w.lastApplied = bytes.Clone(next)
	w.lastGood = bytes.Clone(next)
	w.writes++
	return true, nil
}

// writeLocked performs the actual write, choosing a strategy from the mount
// mode.
//
// Both strategies end with the file fsynced before Apply reports success, so a
// reported success survives a power cut. See writeInPlace for what a crash can
// leave behind in "file" mode and why.
func (w *Writer) writeLocked(data []byte) error {
	switch w.mode {
	case config.MountModeDir:
		return w.writeAtomic(data)
	case config.MountModeFile:
		return w.writeInPlace(data)
	default:
		return fmt.Errorf("unknown mount mode %q", w.mode)
	}
}

// writeAtomic writes a sibling temporary file and renames it over the target.
// This is fully atomic, but rename(2) fails with EBUSY when the target is
// itself a bind mount, which is why it is only used in "dir" mode.
func (w *Writer) writeAtomic(data []byte) error {
	// A hosts file that is a symlink (to /run, say) must stay one: renaming over
	// the link would replace it with a regular file. The target is written
	// instead, next to where it really lives.
	target := w.path
	if resolved, err := filepath.EvalSymlinks(w.path); err == nil {
		target = resolved
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// The replacement inherits the mode and owner of the file it replaces. A
	// hosts file is not always 0644 root:root, and silently changing who can
	// read or write it is a side effect nobody asked for.
	perm := DefaultPerm
	var uid, gid int
	haveOwner := false
	if info, err := os.Stat(target); err == nil {
		perm = info.Mode().Perm()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid, haveOwner = int(st.Uid), int(st.Gid), true
		}
	}

	tmp, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Any failure past this point must not leave the temp file behind.
	//
	// The cleanup errors are deliberately discarded and this is the one place
	// in the package where that is right. The operation has already failed,
	// so its own error is the only one an operator can act on; a failure to
	// close or unlink a temp file would only add noise. Stating the reason
	// here is better than a blanket nolint, because it survives the next
	// reader wondering whether it is an oversight.
	cleanup := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return cause
	}

	if err := tmp.Chmod(perm); err != nil {
		return cleanup(fmt.Errorf("chmod %s: %w", tmpName, err))
	}
	if haveOwner {
		// Only a process that may change ownership can do it, and one that
		// cannot (no CAP_CHOWN) is, by construction, already writing as the
		// owner it was given. A refusal is therefore not a failure.
		if err := tmp.Chown(uid, gid); err != nil && !errors.Is(err, fs.ErrPermission) {
			return cleanup(fmt.Errorf("chown %s: %w", tmpName, err))
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(fmt.Errorf("write %s: %w", tmpName, err))
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync %s: %w", tmpName, err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) // see cleanup above: the real error is the one above
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName) // see cleanup above: the real error is the one above
		return fmt.Errorf("rename %s to %s: %w", tmpName, target, err)
	}

	// Sync the directory so the rename itself survives a power cut.
	//
	// A failure here is reported rather than swallowed. The rename has
	// already happened, so the file on disk is correct right now, but its
	// directory entry may not be durable: a power cut could bring back the
	// previous version. The caller logs it and the next reconcile retries,
	// which is the right response. Silently returning success would leave the
	// durability guarantee silently broken.
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync %s after renaming into it: %w", dir, err)
	}
	return nil
}

// writeInPlace overwrites the target without renaming it.
//
// The flock is what makes this safe against a second instance of the agent;
// the single write is what keeps the visible window of incoherence down to
// one syscall.
func (w *Writer) writeInPlace(data []byte) (err error) {
	f, err := os.OpenFile(w.path, os.O_RDWR|os.O_CREATE, DefaultPerm)
	if err != nil {
		return fmt.Errorf("open the hosts file for writing: %w", err)
	}
	defer func() {
		// The explicit unlock is best effort: the kernel drops the flock when
		// the descriptor is closed, so a failure here cannot leave it held.
		// A failed close is reported, unless an earlier error is already.
		_ = unlock(f)
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close the hosts file: %w", cerr)
		}
	}()

	if err := flockExclusive(f); err != nil {
		return fmt.Errorf("lock %s: %w", w.path, err)
	}

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat the hosts file: %w", err)
	}

	// When the new content is shorter, the file is cut to its final length
	// first and the content is written over it afterwards.
	//
	// The order matters for what a crash can leave behind. Cutting afterwards
	// would leave, for the instant between the two calls, the new content
	// followed by the tail of the old one: complete lines after the closing
	// marker that nothing could ever tell apart from the operator's own, and
	// that would then be kept for good. Cutting first leaves only a prefix of
	// the old file: the operator's lines are untouched and the block is
	// missing its end marker, which is exactly the damage recovery recognises
	// and rebuilds. A longer or equal content needs no cut at all.
	if int64(len(data)) < info.Size() {
		if err := f.Truncate(int64(len(data))); err != nil {
			return fmt.Errorf("truncate the hosts file: %w", err)
		}
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return fmt.Errorf("write the hosts file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync the hosts file: %w", err)
	}
	return nil
}

// Clear removes the managed block and leaves everything else as it was.
//
// It is what the agent does when it stops: records that nothing keeps up to
// date would sooner or later point at an address that has moved to another
// container. It is idempotent, and a file without a block, or no file at all,
// is not touched.
func (w *Writer) Clear() (changed bool, err error) {
	return w.Apply(nil)
}

// CleanStale removes temporary files left in the hosts file's directory by a
// writer that was killed between creating one and renaming it. Only "dir" mode
// creates them. It returns how many it removed.
func (w *Writer) CleanStale() (removed int, err error) {
	if w.mode != config.MountModeDir {
		return 0, nil
	}
	dir := filepath.Dir(w.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("list %s: %w", dir, err)
	}

	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), TempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleTempAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// Repair restores the last known good content when the file on disk shows
// signs of a crash. It is the recovery entry point and is safe to call on
// every start.
//
// Damage is detected structurally rather than by trying to parse the file.
// That distinction matters: a truncated hosts line such as "127.0.0.1 nginx.d"
// is still syntactically plausible, so a parse-based check would never fire,
// while a crash always breaks the one invariant this writer controls, namely
// that a written block is closed by its EndMarker.
//
// The checks are:
//   - the file is missing, empty, or unreadable;
//   - a block we previously wrote is now absent or unterminated.
//
// It reports whether it had to intervene.
func (w *Writer) Repair() (repaired bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.lastGood == nil {
		// Nothing to fall back to. The file is either absent or was never
		// read successfully, so leave it alone and let Apply recreate it.
		return false, nil
	}

	healthy := func(f *File) bool {
		if f.BlockTruncated {
			return false
		}
		// If the baseline carries a block, the file on disk must still have
		// a complete one. Its absence is the fingerprint of a lost write.
		if baselineHasBlock(w.lastGood) && !f.HadBlock {
			return false
		}
		return true
	}

	data, rerr := os.ReadFile(w.path)
	if rerr == nil && bytes.Equal(data, w.lastGood) {
		// The file is exactly the baseline. Restoring it would rewrite the
		// same bytes and report a repair that never happened; whatever is
		// wrong with it is for Apply to rebuild, since a baseline taken from
		// the damaged file has nothing better to offer.
		return false, nil
	}
	switch {
	case rerr != nil && errors.Is(rerr, fs.ErrNotExist):
		// fall through to restore
	case rerr != nil:
		return false, fmt.Errorf("read the hosts file: %w", rerr)
	case len(data) == 0:
		// fall through to restore
	default:
		f, perr := Parse(bytes.NewReader(data))
		if perr != nil {
			return false, fmt.Errorf("parse %s: %w", w.path, perr)
		}
		if healthy(f) {
			return false, nil
		}
	}

	if err := w.writeLocked(w.lastGood); err != nil {
		return false, fmt.Errorf("restore %s: %w", w.path, err)
	}
	w.lastApplied = bytes.Clone(w.lastGood)
	return true, nil
}

// baselineHasBlock reports whether the remembered content carries a managed
// block.
func baselineHasBlock(baseline []byte) bool {
	if len(baseline) == 0 {
		return false
	}
	f, err := Parse(bytes.NewReader(baseline))
	if err != nil {
		return false
	}
	return f.HadBlock && !f.BlockTruncated
}

// salvageOrphans returns the lines of an unterminated block that are not ours.
//
// A crash leaves a block without its end marker, and the writer's records are
// at the end of the file, so the orphan normally holds nothing but our own
// header and records. But the same shape appears when someone deletes the end
// marker by hand and keeps editing, and then the operator's own lines sit
// inside it. Dropping them would break the one promise this agent makes, so
// only what is recognisably ours is discarded: the generated header and the
// address lines under the owned suffix.
func (w *Writer) salvageOrphans(orphans []Line) []Line {
	var keep []Line
	for _, l := range orphans {
		switch l.Kind {
		case LineComment:
			if isHeaderLine(l.Raw) {
				continue
			}
		case LineEntry:
			if w.owns(Entry{IP: l.IP, Names: l.Names}) {
				continue
			}
		}
		keep = append(keep, l)
	}
	return keep
}

// owns reports whether an entry belongs to this agent: every name is under the
// owned suffix. Without a configured suffix every entry is assumed to be ours,
// which is the conservative choice for a block this agent wrote itself.
func (w *Writer) owns(e Entry) bool {
	if w.suffix == "" {
		return true
	}
	if len(e.Names) == 0 {
		return false
	}
	for _, n := range e.Names {
		if !strings.HasSuffix(strings.ToLower(n), "."+w.suffix) {
			return false
		}
	}
	return true
}

func isHeaderLine(raw string) bool {
	t := strings.TrimSpace(raw)
	for _, h := range headerLines {
		if t == h {
			return true
		}
	}
	return false
}

// syncDir fsyncs a directory so that a rename performed inside it becomes
// durable. Renames are metadata operations: the data blocks are already
// synced, but without this the directory entry itself may be lost on a power
// cut, which would resurrect the previous version of the file.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is the directory of the configured hosts file, not user input
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() {
		// A close failure on a read-only directory handle has no bearing on
		// the sync that already happened, so it must not mask the real error.
		_ = d.Close()
	}()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}

func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlock releases the advisory lock. It is used from a defer, where there is
// no way to act on a failure, so the error is returned for the caller to
// ignore deliberately rather than discarded at the call site.
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
