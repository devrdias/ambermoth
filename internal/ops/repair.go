package ops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// RepairAction says what Repair did, or with dryRun what it would do.
type RepairAction string

// The actions of Repair.
const (
	RepairNone    RepairAction = "none"    // nothing to repair
	RepairNewline RepairAction = "newline" // the last entry was whole and only lacked its newline
	RepairCut     RepairAction = "cut"     // a torn last line was removed and a gap entry appended
	RepairGap     RepairAction = "gap"     // an earlier repair removed a line but never recorded it
)

// ErrRepairRefused is returned by Repair when the ledger is damaged in a way
// repair does not touch. Nothing was changed.
var ErrRepairRefused = errors.New("repair refused")

// RepairResult is what Repair did, or would do.
type RepairResult struct {
	Action         RepairAction `json:"action"`
	DryRun         bool         `json:"dry_run"`
	LastSeq        int64        `json:"last_seq"`                 // last intact entry
	RemovedBytes   int          `json:"removed_bytes,omitempty"`  // size of the removed line
	RemovedSHA256  string       `json:"removed_sha256,omitempty"` // and its hash
	Sidecar        string       `json:"sidecar,omitempty"`        // file name next to the ledger that holds it
	Gaps           int          `json:"gaps"`                     // gap entries appended
	AnchorProblems int          `json:"anchor_problems"`          // anchors that fail; not a reason to refuse
}

// causePrefix starts the cause of every gap entry Repair writes.
const causePrefix = "torn final line removed by repair: "

// tornInfix sits between the ledger file name and the seq in a sidecar name.
const tornInfix = ".torn-"

// Repair recovers a ledger whose last line was cut short by a crash. A torn
// last line (bytes after the final newline that are not a whole entry) is
// copied to a file next to the ledger, cut off, and answered by a gap entry
// that gives its size and hash. A last entry that is whole but lacks its
// newline only gets the newline. Complete entries are never edited or removed:
// anything else wrong with the ledger is refused with ErrRepairRefused.
//
// Repair holds the ledger lock while it changes the file and returns
// ledger.ErrLocked, wrapped, if a writer has it. It can be run again after an
// interruption: a line that was saved but not yet recorded is picked up and
// recorded. With dryRun it only reports, and takes no lock.
func Repair(dir string, clock ledger.Clock, dryRun bool) (*RepairResult, error) {
	if clock == nil {
		return nil, ledger.ErrNilClock
	}
	run := &repairRun{dir: dir, clock: clock, dryRun: dryRun, path: filepath.Join(dir, ledger.LedgerFile),
		res: &RepairResult{Action: RepairNone, DryRun: dryRun}}
	if err := run.fixFile(); err != nil {
		return nil, err
	}
	if dryRun || len(run.owed) == 0 {
		return run.res, nil
	}
	if err := run.appendGaps(); err != nil {
		return nil, err
	}
	return run.res, nil
}

// repairRun is the state of one Repair call.
type repairRun struct {
	dir    string
	path   string
	clock  ledger.Clock
	dryRun bool
	res    *RepairResult
	lastTS string
	owed   []sidecar // saved lines the ledger has no gap entry for yet
	cut    *sidecar  // the line this run cut off, if any
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRepairRefused, fmt.Sprintf(format, a...))
}

// fixFile does the part of the repair that needs the ledger lock: it checks
// the ledger and fixes its last line. The gap entries come after, through the
// store, which takes the same lock.
func (r *repairRun) fixFile() error {
	unlock, err := r.lock()
	if err != nil {
		return err
	}
	defer unlock()
	f, err := os.Open(r.path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	tl, err := readTail(f)
	if err != nil {
		return err
	}
	sc, err := r.check(f, tl)
	if err != nil {
		return err
	}
	r.res.LastSeq, r.res.AnchorProblems, r.lastTS = sc.lastSeq, sc.anchorProblems, sc.lastTS
	if r.owed, err = owedSidecars(r.dir, sc); err != nil {
		return err
	}
	if err := r.fixTail(tl, sc); err != nil {
		return err
	}
	r.finishResult()
	return nil
}

// lock takes the ledger lock, unless this is a dry run, and returns its release.
func (r *repairRun) lock() (func(), error) {
	if r.dryRun {
		return func() {}, nil
	}
	unlock, err := ledger.TryLock(r.dir)
	if err != nil {
		return nil, fmt.Errorf("repair %s: %w (is the proxy writing? stop it and run repair again)", r.dir, err)
	}
	return unlock, nil
}

// tail is the end of the ledger file: what follows its last newline.
type tail struct {
	size int64
	cut  int64  // offset where the unterminated last line starts; size when there is none
	frag []byte // that line
	kind tailKind
}

type tailKind int

const (
	tailNone  tailKind = iota // the file ends in a newline
	tailEntry                 // a whole JSON object with no newline
	tailTorn                  // anything else
)

func readTail(f *os.File) (tail, error) {
	st, err := f.Stat()
	if err != nil {
		return tail{}, err
	}
	size := st.Size()
	if size == 0 {
		return tail{}, refuse("%s is empty: there is no ledger to recover", ledger.LedgerFile)
	}
	cut, err := findCut(f, size)
	if err != nil {
		return tail{}, err
	}
	frag := make([]byte, size-cut)
	if _, err := f.ReadAt(frag, cut); err != nil && !errors.Is(err, io.EOF) {
		return tail{}, err
	}
	return tail{size: size, cut: cut, frag: frag, kind: classify(frag)}, nil
}

func classify(frag []byte) tailKind {
	switch {
	case len(frag) == 0:
		return tailNone
	case ledger.ParseEntry(frag) != nil:
		return tailEntry
	}
	return tailTorn
}

// findCut returns the offset just after the last newline in the first size
// bytes of f, or 0 when there is none.
func findCut(f *os.File, size int64) (int64, error) {
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	for end := size; end > 0; {
		start := max(end-chunk, 0)
		n := end - start
		if _, err := f.ReadAt(buf[:n], start); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		if end = start; size-end > ledger.MaxLineBytes {
			return 0, refuse("the last line is over %d MiB, too long to be one torn entry; run 'ambermoth verify'", ledger.MaxLineBytes>>20)
		}
	}
	return 0, nil
}

// scan is what a verification pass learned about the ledger.
type scan struct {
	lastSeq        int64
	lastTS         string
	causes         []string // the cause of every gap entry
	anchorProblems int
}

func (s *scan) note(e ledger.Entry, _ []byte, _ int64) {
	if e == nil {
		return
	}
	s.lastTS = strings.Clone(e.Str("ts"))
	if e.Type() == ledger.TypeGap {
		s.causes = append(s.causes, strings.Clone(e.Str("cause")))
	}
}

// check verifies what repair relies on: the whole ledger when the last line is
// a whole entry (with its newline added), everything before the last line when
// it is torn. A chain error refuses the repair; a bad anchor does not.
func (r *repairRun) check(f *os.File, tl tail) (*scan, error) {
	var in io.Reader = io.NewSectionReader(f, 0, tl.size)
	switch tl.kind {
	case tailTorn:
		if tl.cut == 0 {
			return nil, refuse("the torn line is the first one, the genesis entry: there is nothing to recover, start a new ledger with 'ambermoth init'")
		}
		in = io.NewSectionReader(f, 0, tl.cut)
	case tailEntry:
		in = io.MultiReader(in, strings.NewReader("\n"))
	}
	sc := &scan{}
	rep, err := ledger.VerifyReader(in, ledger.VerifyOptions{AnchorDir: filepath.Join(r.dir, ledger.AnchorDir), OnEntry: sc.note})
	if err != nil {
		return nil, err
	}
	var chain []string
	for _, e := range rep.Errors {
		if strings.HasPrefix(e, "anchor seq ") {
			sc.anchorProblems++
			continue
		}
		chain = append(chain, e)
	}
	if len(chain) > 0 {
		return nil, refuse("the ledger fails verification (%s); repair only cuts a torn last line, run 'ambermoth verify' for the report", chain[0])
	}
	sc.lastSeq = rep.LastSeq
	return sc, nil
}

func (r *repairRun) fixTail(tl tail, sc *scan) error {
	switch tl.kind {
	case tailEntry:
		r.res.Action = RepairNewline
		return r.addNewline()
	case tailTorn:
		r.res.Action = RepairCut
		return r.cutTail(tl, sc)
	}
	return nil
}

func (r *repairRun) addNewline() error {
	if r.dryRun {
		return nil
	}
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("\n"); err != nil {
		_ = f.Close()
		return err
	}
	return syncClose(f)
}

// cutTail saves the torn line and cuts it off. A saved copy from an earlier,
// interrupted run is reused, so one torn line gives one sidecar and one gap.
func (r *repairRun) cutTail(tl tail, sc *scan) error {
	seq := sc.lastSeq + 1
	sd, ok := findOwed(r.owed, seq, tl.frag)
	if !ok {
		sd = sidecar{seq: seq, data: tl.frag, name: fmt.Sprintf("%s%s%d-%d", ledger.LedgerFile, tornInfix, seq, r.clock().Unix())}
		if !r.dryRun {
			var err error
			if sd.name, err = writeSidecar(r.dir, sd.name, sd.data); err != nil {
				return fmt.Errorf("saving the torn line: %w", err)
			}
		}
		r.owed = append(r.owed, sd)
	}
	r.cut = &sd
	if r.dryRun {
		return nil
	}
	return truncateLedger(r.path, tl.size, tl.cut)
}

func findOwed(owed []sidecar, seq int64, data []byte) (sidecar, bool) {
	for _, s := range owed {
		if s.seq == seq && bytes.Equal(s.data, data) {
			return s, true
		}
	}
	return sidecar{}, false
}

// finishResult describes the line cut by this run or, when there is none, the
// first one still owed a gap entry.
func (r *repairRun) finishResult() {
	r.res.Gaps = len(r.owed)
	if len(r.owed) == 0 {
		return
	}
	sort.Slice(r.owed, func(i, j int) bool {
		a, b := r.owed[i], r.owed[j]
		return a.seq < b.seq || a.seq == b.seq && a.name < b.name
	})
	sd := r.owed[0]
	if r.cut != nil {
		sd = *r.cut
	} else if r.res.Action == RepairNone {
		r.res.Action = RepairGap
	}
	r.res.RemovedBytes, r.res.RemovedSHA256, r.res.Sidecar = len(sd.data), ledger.SHA256Bytes(sd.data), sd.name
}

// appendGaps records every saved line the ledger has no gap entry for, through
// the store, so the entries get their seq and hashes the normal way and are
// fsynced.
func (r *repairRun) appendGaps() error {
	s, err := ledger.Open(r.dir, r.clock)
	if err != nil {
		return fmt.Errorf("the torn line is saved but its gap entry is not written, %w; fix that and run repair again", err)
	}
	until := ledger.FormatTime(s.Now())
	since := r.lastTS
	if since == "" {
		since = until
	}
	gaps := make([]ledger.Entry, 0, len(r.owed))
	for _, sd := range r.owed {
		gaps = append(gaps, ledger.Entry{"type": ledger.TypeGap, "dropped": int64(1), "since": since, "until": until, "cause": sd.cause()})
	}
	if err := s.Append(gaps...); err != nil {
		return fmt.Errorf("the torn line is saved but its gap entry is not written, %w; run repair again", err)
	}
	return nil
}

// sidecar is a removed line kept in a file next to the ledger.
type sidecar struct {
	name string // file name, ledger.jsonl.torn-<seq>-<unix time>[.<n>]
	seq  int64  // the seq the line would have had: the last intact seq plus one
	data []byte
}

// cause is the text of the gap entry that records the line. It gives the size
// and hash, never the bytes, and names the file that has them.
func (s sidecar) cause() string {
	return fmt.Sprintf("%s%d bytes, %s, after seq %d, saved as %s", causePrefix, len(s.data), ledger.SHA256Bytes(s.data), s.seq-1, s.name)
}

// recordedBy reports whether one of the gap causes names this sidecar.
func (s sidecar) recordedBy(causes []string) bool {
	for _, c := range causes {
		if strings.HasPrefix(c, causePrefix) && strings.HasSuffix(c, "saved as "+s.name) {
			return true
		}
	}
	return false
}

// owedSidecars lists the sidecars in dir that no gap entry names. One that
// belongs to a longer ledger than this one is not ours and is left out.
func owedSidecars(dir string, sc *scan) ([]sidecar, error) {
	paths, err := filepath.Glob(filepath.Join(dir, ledger.LedgerFile+tornInfix+"*"))
	if err != nil {
		return nil, err
	}
	var owed []sidecar
	for _, p := range paths {
		sd, ok := readSidecar(p)
		if ok && sd.seq <= sc.lastSeq+1 && !sd.recordedBy(sc.causes) {
			owed = append(owed, sd)
		}
	}
	return owed, nil
}

// readSidecar reads a sidecar; ok is false for a file that is not one.
func readSidecar(path string) (sidecar, bool) {
	name := filepath.Base(path)
	rest := strings.TrimPrefix(name, ledger.LedgerFile+tornInfix)
	num, _, found := strings.Cut(rest, "-")
	seq, err := strconv.ParseInt(num, 10, 64)
	if !found || err != nil || seq < 1 {
		return sidecar{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sidecar{}, false
	}
	return sidecar{name: name, seq: seq, data: data}, true
}

// writeSidecar creates a new file with data, mode 0600, and returns its name.
// It never overwrites: when base exists it tries base.1, base.2 and so on.
func writeSidecar(dir, base string, data []byte) (string, error) {
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s.%d", base, i)
		}
		err := createSynced(filepath.Join(dir, name), data)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		syncDir(dir)
		return name, nil
	}
	return "", fmt.Errorf("no free name for %s", base)
}

func createSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := syncClose(f); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// syncDir makes a new directory entry durable. Where a directory cannot be
// synced the error is dropped: the file itself was synced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func syncClose(f *os.File) error {
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// truncateLedger cuts the ledger at offset cut and fsyncs. It checks the size
// first, so a file that grew since it was read is left alone.
func truncateLedger(path string, size, cut int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil || st.Size() != size {
		_ = f.Close()
		return fmt.Errorf("%s changed while it was being repaired; nothing was cut", ledger.LedgerFile)
	}
	if err := f.Truncate(cut); err != nil {
		_ = f.Close()
		return err
	}
	return syncClose(f)
}
