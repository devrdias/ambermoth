package pack_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/canon"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/pack"
	"github.com/devrdias/ambermoth/internal/report"
)

// goodPack is a verified pack and the env that made it.
type goodPack struct {
	e    *env
	path string
}

// newMidPack is a pack whose segment starts and ends at anchors and has one
// context entry:
//
//	1 key_event (context)   2 call   3 anchor(2)   4 call   5 call   6 anchor(5)
//
// The segment is seq 2..6.
func newMidPack(t *testing.T) goodPack {
	t.Helper()
	e := newEnv(t)
	e.keyEvent(0)
	e.call(1, "anthropic", "prod")
	e.anchor(1, 1)
	e.call(2, "anthropic", "prod")
	e.call(2, "anthropic", "eval")
	e.anchor(2, 1)
	o := period(e.opts(), 2, 2)
	e.build(o)
	return goodPack{e, o.Out}
}

// newGenesisPack is a pack from genesis with an anchor inside the segment
// and an unanchored tail:
//
//	0 genesis   1 call   2 call   3 anchor(2)   4 call
func newGenesisPack(t *testing.T) goodPack {
	t.Helper()
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	e.call(2, "anthropic", "prod")
	e.anchor(2, 1)
	e.call(3, "anthropic", "prod")
	o := period(e.opts(), 1, 3)
	e.build(o)
	return goodPack{e, o.Out}
}

// newBarePack is a pack from a ledger that was never anchored.
func newBarePack(t *testing.T) goodPack {
	t.Helper()
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	e.call(2, "anthropic", "prod")
	o := period(e.opts(), 1, 2)
	e.build(o)
	return goodPack{e, o.Out}
}

// draft is a pack taken apart for editing.
type draft struct {
	t     *testing.T
	files map[string][]byte
	m     *pack.Manifest
}

func (g goodPack) draft(t *testing.T) *draft {
	t.Helper()
	d := &draft{t: t, files: readPack(t, g.path), m: &pack.Manifest{}}
	if err := json.Unmarshal(d.files["manifest.json"], d.m); err != nil {
		t.Fatal(err)
	}
	return d
}

// segment parses ledger/segment.jsonl.
func (d *draft) segment() []ledger.Entry {
	d.t.Helper()
	es, _, err := ledger.Read(bytes.NewReader(d.files["ledger/segment.jsonl"]))
	if err != nil {
		d.t.Fatal(err)
	}
	return es
}

// rechain lets edit change the segment's entries, then links and hashes them
// again, so only the checks that do not rely on the chain alone can notice.
// The manifest's last_hash follows the new head.
func (d *draft) rechain(edit func(es []ledger.Entry) []ledger.Entry) {
	d.t.Helper()
	es := edit(d.segment())
	var out bytes.Buffer
	for i, e := range es {
		if i > 0 {
			e["prev_hash"] = es[i-1].Hash()
		}
		delete(e, "entry_hash")
		h, err := ledger.ComputeHash(e)
		if err != nil {
			d.t.Fatal(err)
		}
		e["entry_hash"] = h
		line, err := canon.Marshal(map[string]any(e))
		if err != nil {
			d.t.Fatal(err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	d.files["ledger/segment.jsonl"] = out.Bytes()
	d.m.Segment.LastHash = es[len(es)-1].Hash()
}

// write saves the draft as a zip. With fix, the manifest lists the files as
// they are now, so the hash check passes and a deeper check has to notice.
func (d *draft) write(fix bool) string {
	d.t.Helper()
	if fix {
		var kept []pack.File
		for _, f := range d.m.Files {
			if b, ok := d.files[f.Path]; ok {
				kept = append(kept, pack.File{Path: f.Path, SHA256: ledger.SHA256Bytes(b), Bytes: int64(len(b))})
			}
		}
		d.m.Files = kept
	}
	mb, err := json.MarshalIndent(d.m, "", "  ")
	if err != nil {
		d.t.Fatal(err)
	}
	d.files["manifest.json"] = append(mb, '\n')
	names := make([]string, 0, len(d.files))
	for n := range d.files {
		names = append(names, n)
	}
	out := filepath.Join(d.t.TempDir(), "bad.zip")
	if err := pack.WriteZip(out, names, d.files, time.Now()); err != nil {
		d.t.Fatal(err)
	}
	return out
}

// lines is what verify-pack reports as errors: the pack's, then the ledger's.
func lines(r *pack.PackReport) []string {
	return append(append([]string{}, r.Errors...), r.Ledger.Errors...)
}

// segmentLine returns the JSON line of the segment entry with the given seq.
func entryWithSeq(es []ledger.Entry, seq int64) ledger.Entry {
	for _, e := range es {
		if e.Seq() == seq {
			return e
		}
	}
	return nil
}

// rerender replaces summary.html with the render the verifier would make from
// the draft as it stands, like a sender who regenerates it after the edit.
func (d *draft) rerender(e *env) {
	d.t.Helper()
	seg, lines, err := ledger.Read(bytes.NewReader(d.files["ledger/segment.jsonl"]))
	if err != nil {
		d.t.Fatal(err)
	}
	var ctx []ledger.Entry
	if cb, ok := d.files["ledger/context.jsonl"]; ok {
		if ctx, _, err = ledger.Read(bytes.NewReader(cb)); err != nil {
			d.t.Fatal(err)
		}
	}
	from, _ := time.Parse("2006-01-02", d.m.Period.From)
	to, _ := time.Parse("2006-01-02", d.m.Period.To)
	f := report.Filter{From: from, To: to.AddDate(0, 0, 1), Provider: d.m.ProviderFilter}
	rep := ledger.Verify(seg, lines, ledger.VerifyOptions{
		LoadToken: func(name string) ([]byte, error) {
			if b, ok := d.files["anchors/"+name]; ok {
				return b, nil
			}
			return nil, os.ErrNotExist
		},
		CheckAnchor: ops.AnchorVerifier(e.auth.Roots, false),
	})
	rm := *d.m
	rm.Totals = report.Summarize(seg, f).Totals
	html, err := pack.RenderSummary(&rm, report.Summarize(append(append([]ledger.Entry{}, ctx...), seg...), f), rep, nil)
	if err != nil {
		d.t.Fatal(err)
	}
	d.files["summary.html"] = html
}
