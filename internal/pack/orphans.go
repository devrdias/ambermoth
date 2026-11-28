package pack

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// FormatV2 is the format of a pack whose segment holds an intent entry (SPEC
// §9.5). A pack with none is Format, as it always was.
const FormatV2 = "ambermoth-pack/2"

// maxOrphansListed is how many orphan intents a manifest and a summary list.
// The count is always complete.
const maxOrphansListed = 50

// knownFormat reports whether this verifier reads format.
func knownFormat(format string) bool { return format == Format || format == FormatV2 }

// OrphanLine is the sentence a pack, a summary and a report use for n intents
// that no call entry completes.
func OrphanLine(n int) string {
	if n == 1 {
		return orphanSingular
	}
	return fmt.Sprintf(orphanPlural, n)
}

// orphanClaim is the count and the listed part of the orphans of a segment, as
// a manifest carries them.
func orphanClaim(orphans []ledger.OrphanIntent) (int, []ledger.OrphanIntent) {
	if len(orphans) == 0 {
		return 0, nil
	}
	return len(orphans), slices.Clone(orphans[:min(len(orphans), maxOrphansListed)])
}

// orphanLine is the sentence for the manifest's orphans, or "" when it has none.
func orphanLine(m *Manifest) string {
	if m.OrphanIntents == 0 {
		return ""
	}
	return OrphanLine(m.OrphanIntents)
}

// orphanNote is the small print under the list of orphans in the summary.
func orphanNote(m *Manifest) string {
	if m.OrphanIntents == 0 {
		return ""
	}
	parts := []string{orphanNearEnd, orphanTotals}
	if m.OrphanIntents > len(m.Orphans) {
		parts = append([]string{fmt.Sprintf(orphanCapped, len(m.Orphans), m.OrphanIntents-len(m.Orphans))}, parts...)
	}
	return strings.Join(parts, " ")
}

// withOrphanParagraph adds the paragraph on orphans to a letter, before the
// one on keys. A manifest with none leaves the letter as it is.
func withOrphanParagraph(letter []byte, m *Manifest) []byte {
	if m.OrphanIntents == 0 {
		return letter
	}
	para := []byte(fmt.Sprintf(orphanLetter, OrphanLine(m.OrphanIntents), orphanNearEnd) + "\n\n")
	before, after, found := bytes.Cut(letter, []byte("**Keys.**"))
	if !found {
		return append(letter, append([]byte("\n"), para...)...)
	}
	out := append(before[:len(before):len(before)], para...)
	return append(append(out, "**Keys.**"...), after...)
}

// checkIntents checks that the format and the segment agree, and then the
// orphans the manifest claims against the ones the segment gives.
func (c *packCheck) checkIntents() {
	n := c.seg.intents
	switch {
	case c.m.Format == Format && n > 0:
		c.errf("manifest format %s but the segment holds %s%s", Format, countIntents(n), formatRule)
	case c.m.Format == FormatV2 && n == 0:
		c.errf("manifest format %s but the segment holds %s%s", FormatV2, countIntents(n), formatRule)
	case knownFormat(c.m.Format):
		c.checkOrphans()
	}
}

// formatRule is said with a format that does not fit the segment.
const formatRule = " (a segment with an intent entry is " + FormatV2 + ", any other is " + Format + ")"

func countIntents(n int) string {
	switch n {
	case 0:
		return "no intent entry"
	case 1:
		return "1 intent entry"
	}
	return fmt.Sprintf("%d intent entries", n)
}

// checkOrphans compares the manifest's orphan count and list with what the
// verification of the segment found.
func (c *packCheck) checkOrphans() {
	count, list := orphanClaim(c.r.Ledger.Orphans)
	if c.m.OrphanIntents != count || !slices.Equal(c.m.Orphans, list) {
		c.errf("orphan intents in manifest do not match the intents recomputed from the ledger segment")
	}
}
