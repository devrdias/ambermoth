package canon

// UTF16Less and SortedKeys expose the key ordering to the black-box tests.
func UTF16Less(a, b string) bool { return utf16Compare(a, b) < 0 }

var SortedKeys = sortedKeys
