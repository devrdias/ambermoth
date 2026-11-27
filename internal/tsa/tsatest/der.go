package tsatest

import (
	"encoding/asn1"
	"testing"
)

// Node is a DER element that tests can edit and encode again. A constructed
// element holds Kids, a primitive one holds Bytes. Raw, when set, is written
// as it is, so a test can plant a broken or truncated encoding.
type Node struct {
	Class, Tag int
	Compound   bool
	Bytes      []byte
	Kids       []*Node
	Raw        []byte
}

// ParseDER reads der into a tree. Primitive contents are not looked into;
// use Inner for an OCTET STRING that wraps more DER.
func ParseDER(t testing.TB, der []byte) *Node {
	t.Helper()
	n, rest, err := parse(der)
	if err != nil || len(rest) > 0 {
		t.Fatalf("tsatest: bad DER (rest %d bytes): %v", len(rest), err)
	}
	return n
}

func parse(der []byte) (*Node, []byte, error) {
	var rv asn1.RawValue
	rest, err := asn1.Unmarshal(der, &rv)
	if err != nil {
		return nil, nil, err
	}
	n := &Node{Class: rv.Class, Tag: rv.Tag, Compound: rv.IsCompound}
	if !rv.IsCompound {
		n.Bytes = append([]byte(nil), rv.Bytes...) // edits must not touch the input
		return n, rest, nil
	}
	for b := rv.Bytes; len(b) > 0; {
		var kid *Node
		if kid, b, err = parse(b); err != nil {
			return nil, nil, err
		}
		n.Kids = append(n.Kids, kid)
	}
	return n, rest, nil
}

// FromValue encodes v with encoding/asn1 and returns it as a Node.
func FromValue(t testing.TB, v any) *Node {
	t.Helper()
	der, err := asn1.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return ParseDER(t, der)
}

// DER encodes the tree.
func (n *Node) DER() []byte {
	if n.Raw != nil {
		return n.Raw
	}
	body := n.Bytes
	if n.Compound {
		body = nil
		for _, k := range n.Kids {
			body = append(body, k.DER()...)
		}
	}
	der, err := asn1.Marshal(asn1.RawValue{Class: n.Class, Tag: n.Tag, IsCompound: n.Compound, Bytes: body})
	if err != nil {
		panic(err) // a RawValue with these fields always encodes
	}
	return der
}

// At follows child indexes from n.
func (n *Node) At(t testing.TB, path ...int) *Node {
	t.Helper()
	for _, i := range path {
		if i >= len(n.Kids) {
			t.Fatalf("tsatest: no child %d (element has %d)", i, len(n.Kids))
		}
		n = n.Kids[i]
	}
	return n
}

// Inner parses the contents of a primitive element (an OCTET STRING holding
// DER) as a tree. SetInner writes an edited tree back.
func (n *Node) Inner(t testing.TB) *Node {
	t.Helper()
	return ParseDER(t, n.Bytes)
}

// SetInner replaces the contents of n with the encoding of inner.
func (n *Node) SetInner(inner *Node) {
	n.Bytes = inner.DER()
}
