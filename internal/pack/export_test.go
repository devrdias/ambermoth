package pack

import "github.com/devrdias/ambermoth/internal/ledger"

// Exported for the black-box tests in this directory.
var WriteZip = writeZip

var CodeLinkFor = codeLinkFor

var RenderSummary = renderSummary

var AddAnchorTokens = addAnchorTokens

var RenderLetter = renderLetter

// CountVerify returns o with every ledger verification Build runs recorded
// in calls, as the number of entries each one covered.
func CountVerify(o Options, calls *[]int) Options {
	o.verify = func(es []ledger.Entry, lines [][]byte, opt ledger.VerifyOptions) *ledger.Report {
		*calls = append(*calls, len(es))
		return ledger.Verify(es, lines, opt)
	}
	return o
}
