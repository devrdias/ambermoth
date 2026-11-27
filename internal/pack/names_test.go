package pack_test

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
)

func TestAddAnchorTokensReadsOnlyPlainFileNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "anchors")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	written := map[string]string{"secret.tsr": "outside", "anchors/2-0-tsa.tsr": "token"}
	for p, b := range written {
		if err := os.WriteFile(filepath.Join(root, p), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	anchor := func(name string) []ledger.Entry {
		return []ledger.Entry{{"type": ledger.TypeCall, "seq": int64(2)}, {"type": ledger.TypeAnchor, "seq": int64(3), "token_file": name}}
	}
	t.Run("adds the token of a plain name", func(t *testing.T) {
		t.Parallel()
		files := map[string][]byte{}
		if err := pack.AddAnchorTokens(files, dir, anchor("2-0-tsa.tsr")); err != nil {
			t.Fatal(err)
		}
		if string(files["anchors/2-0-tsa.tsr"]) != "token" {
			t.Errorf("files = %v", files)
		}
	})
	names := []string{"../secret.tsr", "sub/2-0-tsa.tsr", `..\secret.tsr`, "/etc/passwd", "./2-0-tsa.tsr", "..", ".", "", "a\x00b"}
	for _, n := range names {
		t.Run("rejects "+strings.ReplaceAll(n, "\x00", "NUL"), func(t *testing.T) {
			t.Parallel()
			files := map[string][]byte{}
			err := pack.AddAnchorTokens(files, dir, anchor(n))
			want := fmt.Sprintf("anchor entry seq 3: token_file %q is not a plain file name", n)
			if err == nil || err.Error() != want {
				t.Errorf("error = %v, want %q", err, want)
			}
			if len(files) != 0 {
				t.Errorf("files = %v, want none", files)
			}
		})
	}
}

// zipOf writes a zip whose entries are made by add.
func zipOf(t *testing.T, add func(zw *zip.Writer)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "z.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	add(zw)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func addFile(t *testing.T, zw *zip.Writer, name, body string) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
}

func TestReadPackRefusesUnsafeEntries(t *testing.T) {
	t.Parallel()
	const unsafe = ": unsafe name (want a relative path with no \"..\" or empty parts and no backslash)"
	cases := []struct {
		name string
		add  func(t *testing.T, zw *zip.Writer)
		want string
	}{
		{"a name with ..", func(t *testing.T, zw *zip.Writer) { addFile(t, zw, "../evil", "x") }, `zip entry "../evil"` + unsafe},
		{"a name with .. inside", func(t *testing.T, zw *zip.Writer) { addFile(t, zw, "anchors/../../evil", "x") }, `zip entry "anchors/../../evil"` + unsafe},
		{"an absolute name", func(t *testing.T, zw *zip.Writer) { addFile(t, zw, "/etc/evil", "x") }, `zip entry "/etc/evil"` + unsafe},
		{"a backslash", func(t *testing.T, zw *zip.Writer) { addFile(t, zw, `anchors\..\evil`, "x") }, `zip entry "anchors\\..\\evil"` + unsafe},
		{"an empty part", func(t *testing.T, zw *zip.Writer) { addFile(t, zw, "anchors//a.tsr", "x") }, `zip entry "anchors//a.tsr"` + unsafe},
		{"a dot part", func(t *testing.T, zw *zip.Writer) { addFile(t, zw, "./manifest.json", "x") }, `zip entry "./manifest.json"` + unsafe},
		{"a duplicate name", func(t *testing.T, zw *zip.Writer) {
			addFile(t, zw, "LIMITS.md", "one")
			addFile(t, zw, "LIMITS.md", "two")
		}, `zip entry "LIMITS.md": appears more than once`},
		{"a symlink", func(t *testing.T, zw *zip.Writer) {
			h := &zip.FileHeader{Name: "LIMITS.md"}
			h.SetMode(os.ModeSymlink | 0o777)
			w, err := zw.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte("/etc/passwd"))
		}, `zip entry "LIMITS.md": not a regular file`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := zipOf(t, func(zw *zip.Writer) { c.add(t, zw) })
			if _, err := pack.ReadPack(path); err == nil || err.Error() != c.want {
				t.Errorf("ReadPack error = %v, want %q", err, c.want)
			}
			if _, err := pack.VerifyPack(path, pack.VerifyOptions{Clock: time.Now}); err == nil || err.Error() != c.want {
				t.Errorf("VerifyPack error = %v, want %q", err, c.want)
			}
		})
	}
}

func TestReadPackKeepsTheNamesOfAGoodPack(t *testing.T) {
	t.Parallel()
	g := newMidPack(t)
	want := zipNames(t, g.path)
	files := readPack(t, g.path)
	if len(files) != len(want) {
		t.Errorf("read %d files, the zip has %d", len(files), len(want))
	}
}
