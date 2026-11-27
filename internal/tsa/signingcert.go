package tsa

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
)

//rules:readonly asn1.ObjectIdentifier is a slice, so Go cannot make these const.
var (
	oidSigningCertificate   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 12}
	oidSigningCertificateV2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
)

var errMalformedCertID = errors.New("tsa: malformed signing-certificate attribute")

// signedAttr is one CMS Attribute.
type signedAttr struct {
	Type   asn1.ObjectIdentifier
	Values asn1.RawValue `asn1:"set"`
}

// certID is the first ESSCertID or ESSCertIDv2 of a signing-certificate attribute.
type certID struct {
	hash         crypto.Hash
	certHash     []byte
	issuerSerial *asn1.RawValue // nil when absent
}

// checkSigningCert requires a signing-certificate attribute (RFC 3161 §2.4.2,
// RFC 5816 §2.2.1) among the signed attributes and checks every one present
// against the certificate that verified the signature.
func checkSigningCert(attrsDER []byte, signer *x509.Certificate) error {
	attrs, err := elements(attrsDER)
	if err != nil {
		return err
	}
	found := 0
	for _, a := range attrs {
		var attr signedAttr
		if _, err := asn1.Unmarshal(a.FullBytes, &attr); err != nil {
			return err
		}
		v2 := attr.Type.Equal(oidSigningCertificateV2)
		if !v2 && !attr.Type.Equal(oidSigningCertificate) {
			continue
		}
		found++
		if err := checkCertAttr(attr.Values, v2, signer); err != nil {
			return err
		}
	}
	if found == 0 {
		return errors.New("tsa: token has no signing-certificate attribute")
	}
	return nil
}

func checkCertAttr(values asn1.RawValue, v2 bool, signer *x509.Certificate) error {
	id, err := firstCertID(values, v2)
	if err != nil {
		return err
	}
	return id.match(signer)
}

// firstCertID reads the first certID of the SigningCertificate (or V2) in a
// signing-certificate attribute's value set. The first one names the signer.
func firstCertID(values asn1.RawValue, v2 bool) (certID, error) {
	vals, err := elements(values.Bytes)
	if err != nil || len(vals) < 1 {
		return certID{}, errMalformedCertID
	}
	fields, err := children(vals[0].FullBytes)
	if err != nil || len(fields) < 1 {
		return certID{}, errMalformedCertID
	}
	ids, err := elements(fields[0].Bytes)
	if err != nil || len(ids) < 1 {
		return certID{}, errMalformedCertID
	}
	return parseCertID(ids[0].FullBytes, v2)
}

// parseCertID reads an ESSCertID (RFC 2634) or, when v2 is set, an ESSCertIDv2
// (RFC 5035), whose hashAlgorithm is left out for SHA-256.
func parseCertID(der []byte, v2 bool) (certID, error) {
	f, err := children(der)
	if err != nil {
		return certID{}, errMalformedCertID
	}
	// SHA-1 only names the certificate here, as ESSCertID defines it; it signs nothing.
	id := certID{hash: crypto.SHA1}
	if v2 {
		id.hash = crypto.SHA256
		if f, id.hash, err = certIDAlgorithm(f); err != nil {
			return certID{}, err
		}
	}
	if len(f) < 1 {
		return certID{}, errMalformedCertID
	}
	if _, err := asn1.Unmarshal(f[0].FullBytes, &id.certHash); err != nil {
		return certID{}, errMalformedCertID
	}
	if len(f) > 1 {
		id.issuerSerial = &f[1]
	}
	return id, nil
}

// certIDAlgorithm takes the optional hashAlgorithm off the front of an
// ESSCertIDv2's fields. Only SHA-2 is accepted.
func certIDAlgorithm(f []asn1.RawValue) ([]asn1.RawValue, crypto.Hash, error) {
	if len(f) < 1 || f[0].Class != asn1.ClassUniversal || f[0].Tag != asn1.TagSequence {
		return f, crypto.SHA256, nil
	}
	alg, err := children(f[0].FullBytes)
	if err != nil || len(alg) < 1 {
		return nil, 0, errMalformedCertID
	}
	var oid asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(alg[0].FullBytes, &oid); err != nil {
		return nil, 0, errMalformedCertID
	}
	h, err := hashFor(oid)
	if err != nil || h == crypto.SHA1 {
		return nil, 0, fmt.Errorf("tsa: unsupported signing-certificate hash algorithm %v", oid)
	}
	return f[1:], h, nil
}

func (c certID) match(signer *x509.Certificate) error {
	h := c.hash.New()
	h.Write(signer.Raw)
	if !bytes.Equal(h.Sum(nil), c.certHash) {
		return errors.New("tsa: signing-certificate hash does not match the signer certificate")
	}
	if c.issuerSerial == nil {
		return nil
	}
	return matchIssuerSerial(*c.issuerSerial, signer)
}

// matchIssuerSerial checks an IssuerSerial against the signer: the serial
// number, and a directoryName in the issuer GeneralNames equal to its issuer.
func matchIssuerSerial(is asn1.RawValue, signer *x509.Certificate) error {
	f, err := children(is.FullBytes)
	if err != nil || len(f) != 2 || !f[0].IsCompound {
		return errMalformedCertID
	}
	var serial *big.Int
	if _, err := asn1.Unmarshal(f[1].FullBytes, &serial); err != nil {
		return errMalformedCertID
	}
	names, err := elements(f[0].Bytes)
	if err != nil {
		return errMalformedCertID
	}
	if serial.Cmp(signer.SerialNumber) != 0 || !hasDirectoryName(names, signer.RawIssuer) {
		return errors.New("tsa: signing-certificate issuerSerial does not match the signer certificate")
	}
	return nil
}

// hasDirectoryName reports whether names holds a directoryName ([4]) equal to name.
func hasDirectoryName(names []asn1.RawValue, name []byte) bool {
	for _, n := range names {
		if n.Class == asn1.ClassContextSpecific && n.Tag == 4 && bytes.Equal(n.Bytes, name) {
			return true
		}
	}
	return false
}
