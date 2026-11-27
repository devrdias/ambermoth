// Package tsa is a minimal, standard-library-only RFC 3161 client and token
// verifier (SPEC §7). It verifies the CMS signature over the TSTInfo using the
// certificate embedded in the token, and optionally chains it to caller-supplied
// trust anchors.
package tsa

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"
)

// Object identifiers used by RFC 3161 and CMS.
//
//rules:readonly asn1.ObjectIdentifier is a slice, so Go cannot make these const.
var (
	oidSHA1          = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidSHA256        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidSHA384        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidSHA512        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}
	oidSignedData    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidTSTInfo       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidMessageDigest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidContentType   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidRSA           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidRSASHA256     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidRSASHA384     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 12}
	oidRSASHA512     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	oidRSASHA1       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 5}
	oidECDSASHA256   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidECDSASHA384   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 3}
	oidECDSASHA512   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 4}
	oidECPublicKey   = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
)

type messageImprint struct {
	HashAlgorithm pkix.AlgorithmIdentifier
	HashedMessage []byte
}

type timeStampReq struct {
	Version        int
	MessageImprint messageImprint
	Nonce          *big.Int `asn1:"optional"`
	CertReq        bool     `asn1:"optional"`
}

// ErrUntrustedChain is returned when a token's signature is valid but its
// signer does not chain to any of the supplied trust anchors.
var ErrUntrustedChain = errors.New("TSA certificate does not chain to a trusted root")

// BuildRequest returns a DER TimeStampReq for a SHA-256 digest.
func BuildRequest(digest []byte) ([]byte, *big.Int, error) {
	if len(digest) != 32 {
		return nil, nil, errors.New("digest must be 32 bytes (SHA-256)")
	}
	nonce, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, nil, err
	}
	der, err := asn1.Marshal(timeStampReq{
		Version: 1,
		MessageImprint: messageImprint{
			HashAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidSHA256, Parameters: asn1.NullRawValue},
			HashedMessage: digest,
		},
		Nonce:   nonce,
		CertReq: true,
	})
	return der, nonce, err
}

// Request asks a TSA to timestamp digest and verifies the reply.
func Request(ctx context.Context, url string, digest []byte, client *http.Client) ([]byte, *TokenInfo, error) {
	reqDER, nonce, err := BuildRequest(digest)
	if err != nil {
		return nil, nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	body, err := post(ctx, client, url, reqDER)
	if err != nil {
		return nil, nil, err
	}
	info, err := Verify(body, digest, nil)
	if err != nil {
		return nil, nil, err
	}
	if err := checkNonce(info, nonce); err != nil {
		return nil, nil, err
	}
	return body, info, nil
}

// post sends a TimeStampReq and returns the body of an HTTP 200 reply.
func post(ctx context.Context, client *http.Client, url string, reqDER []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqDER))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/timestamp-query")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TSA %s returned HTTP %d", url, resp.StatusCode)
	}
	return body, nil
}

func checkNonce(info *TokenInfo, nonce *big.Int) error {
	if info.Nonce == nil || info.Nonce.Cmp(nonce) != 0 {
		return errors.New("TSA reply nonce does not match request")
	}
	return nil
}

// TokenInfo is what a verified token asserts.
type TokenInfo struct {
	GenTime       time.Time
	Serial        *big.Int
	Policy        asn1.ObjectIdentifier
	Nonce         *big.Int
	Signer        string // subject of the signing certificate
	ChainVerified bool   // true only if roots were supplied and the chain verified
	SignerCert    *x509.Certificate
}

// elements splits the contents of a constructed DER element into its children.
func elements(b []byte) ([]asn1.RawValue, error) {
	var out []asn1.RawValue
	for len(b) > 0 {
		var rv asn1.RawValue
		rest, err := asn1.Unmarshal(b, &rv)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
		b = rest
	}
	return out, nil
}

func children(der []byte) ([]asn1.RawValue, error) {
	var rv asn1.RawValue
	rest, err := asn1.Unmarshal(der, &rv)
	if err != nil {
		return nil, err
	}
	if len(rest) > 0 {
		return nil, errors.New("trailing data")
	}
	if !rv.IsCompound {
		return nil, errors.New("expected constructed element")
	}
	return elements(rv.Bytes)
}

// oidHash maps a digest algorithm OID to its hash.
type oidHash struct {
	oid  asn1.ObjectIdentifier
	hash crypto.Hash
}

//rules:readonly lookup table, never written.
var digestAlgorithms = []oidHash{
	{oidSHA256, crypto.SHA256}, {oidSHA384, crypto.SHA384}, {oidSHA512, crypto.SHA512}, {oidSHA1, crypto.SHA1},
}

func hashFor(oid asn1.ObjectIdentifier) (crypto.Hash, error) {
	for _, d := range digestAlgorithms {
		if oid.Equal(d.oid) {
			return d.hash, nil
		}
	}
	return 0, fmt.Errorf("unsupported digest algorithm %v", oid)
}

// sigAlgorithm maps a signatureAlgorithm OID, alone or (for a bare key
// algorithm OID) together with the digest OID, to an x509 algorithm.
type sigAlgorithm struct {
	sig, digest asn1.ObjectIdentifier // digest is nil when sig names the hash
	alg         x509.SignatureAlgorithm
}

//rules:readonly lookup table, never written.
var signatureAlgorithms = []sigAlgorithm{
	{sig: oidRSASHA256, alg: x509.SHA256WithRSA},
	{sig: oidRSASHA384, alg: x509.SHA384WithRSA},
	{sig: oidRSASHA512, alg: x509.SHA512WithRSA},
	{sig: oidRSASHA1, alg: x509.SHA1WithRSA},
	{sig: oidECDSASHA256, alg: x509.ECDSAWithSHA256},
	{sig: oidECDSASHA384, alg: x509.ECDSAWithSHA384},
	{sig: oidECDSASHA512, alg: x509.ECDSAWithSHA512},
	// Some TSAs put the key algorithm in signatureAlgorithm; the hash then
	// comes from digestAlgorithm.
	{sig: oidRSA, digest: oidSHA256, alg: x509.SHA256WithRSA},
	{sig: oidRSA, digest: oidSHA384, alg: x509.SHA384WithRSA},
	{sig: oidRSA, digest: oidSHA512, alg: x509.SHA512WithRSA},
	{sig: oidECPublicKey, digest: oidSHA256, alg: x509.ECDSAWithSHA256},
	{sig: oidECPublicKey, digest: oidSHA384, alg: x509.ECDSAWithSHA384},
	{sig: oidECPublicKey, digest: oidSHA512, alg: x509.ECDSAWithSHA512},
}

func sigAlg(digest, sig asn1.ObjectIdentifier) (x509.SignatureAlgorithm, error) {
	for _, a := range signatureAlgorithms {
		if sig.Equal(a.sig) && (a.digest == nil || digest.Equal(a.digest)) {
			return a.alg, nil
		}
	}
	return 0, fmt.Errorf("unsupported signature algorithm %v/%v", sig, digest)
}

// Verify parses a DER TimeStampResp, checks status, imprint and CMS signature,
// and — if roots is non-nil — chains the signer to roots for time-stamping.
func Verify(respDER, expectedDigest []byte, roots *x509.CertPool) (*TokenInfo, error) {
	token, err := grantedToken(respDER)
	if err != nil {
		return nil, err
	}
	return verifyToken(token, expectedDigest, roots)
}

// grantedToken returns the DER ContentInfo of a TimeStampResp whose status
// is granted or granted with modifications.
func grantedToken(respDER []byte) ([]byte, error) {
	top, err := children(respDER)
	if err != nil || len(top) < 1 {
		return nil, fmt.Errorf("not a TimeStampResp: %v", err)
	}
	if err := checkStatus(top[0].FullBytes); err != nil {
		return nil, err
	}
	if len(top) < 2 {
		return nil, errors.New("TimeStampResp has no token")
	}
	return top[1].FullBytes, nil
}

func checkStatus(statusInfoDER []byte) error {
	status, err := children(statusInfoDER)
	if err != nil || len(status) < 1 {
		return errors.New("malformed PKIStatusInfo")
	}
	var st int
	if _, err := asn1.Unmarshal(status[0].FullBytes, &st); err != nil {
		return err
	}
	if st != 0 && st != 1 {
		return fmt.Errorf("TSA refused the request (status %d)", st)
	}
	return nil
}

func verifyToken(ciDER, expectedDigest []byte, roots *x509.CertPool) (*TokenInfo, error) {
	parts, err := signedDataParts(ciDER)
	if err != nil {
		return nil, err
	}
	// parts: version, digestAlgorithms, encapContentInfo, [0] certs?, [1] crls?, signerInfos
	tstDER, err := encapsulatedTSTInfo(parts[2])
	if err != nil {
		return nil, err
	}
	info, err := parseImprintedTSTInfo(tstDER, expectedDigest)
	if err != nil {
		return nil, err
	}
	certs, signerInfo, err := certsAndSigner(parts[3:])
	if err != nil {
		return nil, err
	}
	signer, err := verifySignerInfo(signerInfo, tstDER, certs)
	if err != nil {
		return nil, err
	}
	info.SignerCert = signer
	info.Signer = signerName(signer)
	if err := checkChain(info, certs, roots); err != nil {
		return nil, err
	}
	return info, nil
}

// parseImprintedTSTInfo parses a TSTInfo and checks that it stamps expectedDigest with SHA-256.
func parseImprintedTSTInfo(tstDER, expectedDigest []byte) (*TokenInfo, error) {
	info, imprint, imprintAlg, err := parseTSTInfo(tstDER)
	if err != nil {
		return nil, err
	}
	if !imprintAlg.Equal(oidSHA256) || !bytes.Equal(imprint, expectedDigest) {
		return nil, errors.New("token's message imprint does not match the anchored hash")
	}
	return info, nil
}

// checkChain chains the signer to roots and records it, unless roots is nil.
func checkChain(info *TokenInfo, certs []*x509.Certificate, roots *x509.CertPool) error {
	if roots == nil {
		return nil
	}
	if err := verifyChain(info.SignerCert, certs, roots, info.GenTime); err != nil {
		return err
	}
	info.ChainVerified = true
	return nil
}

// signedDataParts unwraps a CMS ContentInfo holding SignedData and returns
// the SignedData fields (at least version, digestAlgorithms,
// encapContentInfo and one more).
func signedDataParts(ciDER []byte) ([]asn1.RawValue, error) {
	ci, err := children(ciDER)
	if err != nil || len(ci) != 2 {
		return nil, errors.New("malformed ContentInfo")
	}
	if !isOID(ci[0], oidSignedData) {
		return nil, errors.New("token is not CMS SignedData")
	}
	return signedDataFields(ci[1])
}

// signedDataFields reads the SignedData SEQUENCE out of its [0] EXPLICIT wrapper.
func signedDataFields(wrapper asn1.RawValue) ([]asn1.RawValue, error) {
	sd, err := elements(wrapper.Bytes)
	if err != nil || len(sd) != 1 {
		return nil, errors.New("malformed SignedData wrapper")
	}
	parts, err := elements(sd[0].Bytes)
	if err != nil || len(parts) < 4 {
		return nil, errors.New("malformed SignedData")
	}
	return parts, nil
}

// isOID reports whether rv is the object identifier want.
func isOID(rv asn1.RawValue, want asn1.ObjectIdentifier) bool {
	var got asn1.ObjectIdentifier
	_, err := asn1.Unmarshal(rv.FullBytes, &got)
	return err == nil && got.Equal(want)
}

// encapsulatedTSTInfo returns the DER TSTInfo inside an encapContentInfo.
func encapsulatedTSTInfo(encap asn1.RawValue) ([]byte, error) {
	eci, err := elements(encap.Bytes)
	if err != nil || len(eci) != 2 {
		return nil, errors.New("missing encapsulated TSTInfo")
	}
	if !isOID(eci[0], oidTSTInfo) {
		return nil, errors.New("encapsulated content is not TSTInfo")
	}
	var tstDER []byte
	if _, err := asn1.Unmarshal(eci[1].Bytes, &tstDER); err != nil {
		return nil, errors.New("malformed eContent")
	}
	return tstDER, nil
}

// certsAndSigner reads the optional certificates and the single SignerInfo
// from the SignedData fields after encapContentInfo. Certificates that do not
// parse are skipped; the signer must still be found among the rest.
func certsAndSigner(rest []asn1.RawValue) ([]*x509.Certificate, []byte, error) {
	var certs []*x509.Certificate
	var signerInfos asn1.RawValue
	for _, p := range rest {
		switch {
		case p.Class == asn1.ClassContextSpecific && p.Tag == 0:
			cs, err := parseCerts(p.Bytes)
			if err != nil {
				return nil, nil, err
			}
			certs = append(certs, cs...)
		case p.Class == asn1.ClassUniversal && p.Tag == asn1.TagSet:
			signerInfos = p
		}
	}
	sis, err := elements(signerInfos.Bytes)
	if err != nil || len(sis) != 1 {
		return nil, nil, errors.New("expected exactly one SignerInfo")
	}
	return certs, sis[0].FullBytes, nil
}

func parseCerts(der []byte) ([]*x509.Certificate, error) {
	cs, err := elements(der)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for _, c := range cs {
		if cert, err := x509.ParseCertificate(c.FullBytes); err == nil {
			certs = append(certs, cert)
		}
	}
	return certs, nil
}

// signerName is "CN (O)", or the full subject when there is no CN.
func signerName(c *x509.Certificate) string {
	name := c.Subject.CommonName
	if len(c.Subject.Organization) > 0 {
		name += " (" + c.Subject.Organization[0] + ")"
	}
	if name == "" {
		return c.Subject.String()
	}
	return name
}

// verifyChain checks that signer chains to roots, for time stamping, at the
// token's own time.
func verifyChain(signer *x509.Certificate, certs []*x509.Certificate, roots *x509.CertPool, at time.Time) error {
	inter := x509.NewCertPool()
	for _, c := range certs {
		inter.AddCert(c)
	}
	_, err := signer.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter,
		CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUntrustedChain, err)
	}
	return nil
}

func parseTSTInfo(der []byte) (*TokenInfo, []byte, asn1.ObjectIdentifier, error) {
	f, err := children(der)
	if err != nil || len(f) < 5 {
		return nil, nil, nil, errors.New("malformed TSTInfo")
	}
	info, mi, err := tstCore(f)
	if err != nil {
		return nil, nil, nil, err
	}
	if info.Nonce, err = tstNonce(f[5:]); err != nil {
		return nil, nil, nil, err
	}
	return info, mi.HashedMessage, mi.HashAlgorithm.Algorithm, nil
}

// tstCore reads the required TSTInfo fields after version: policy,
// messageImprint, serialNumber and genTime.
func tstCore(f []asn1.RawValue) (*TokenInfo, messageImprint, error) {
	var mi messageImprint
	info := &TokenInfo{Serial: new(big.Int)}
	if _, err := asn1.Unmarshal(f[1].FullBytes, &info.Policy); err != nil {
		return nil, mi, err
	}
	if _, err := asn1.Unmarshal(f[2].FullBytes, &mi); err != nil {
		return nil, mi, err
	}
	if _, err := asn1.Unmarshal(f[3].FullBytes, &info.Serial); err != nil {
		return nil, mi, err
	}
	if _, err := asn1.UnmarshalWithParams(f[4].FullBytes, &info.GenTime, "generalized"); err != nil {
		return nil, mi, fmt.Errorf("genTime: %w", err)
	}
	return info, mi, nil
}

// tstNonce returns the last INTEGER among the optional TSTInfo fields, or nil.
func tstNonce(optional []asn1.RawValue) (*big.Int, error) {
	var nonce *big.Int
	for _, e := range optional {
		if e.Class == asn1.ClassUniversal && e.Tag == asn1.TagInteger {
			nonce = new(big.Int)
			if _, err := asn1.Unmarshal(e.FullBytes, &nonce); err != nil {
				return nil, fmt.Errorf("nonce: %w", err)
			}
		}
	}
	return nonce, nil
}

type issuerAndSerial struct {
	Issuer asn1.RawValue
	Serial *big.Int
}

func verifySignerInfo(der, content []byte, certs []*x509.Certificate) (*x509.Certificate, error) {
	f, err := children(der)
	if err != nil || len(f) < 5 {
		return nil, errors.New("malformed SignerInfo")
	}
	signer, err := findSigner(f[1], certs)
	if err != nil {
		return nil, err
	}
	var digAlg pkix.AlgorithmIdentifier
	if _, err := asn1.Unmarshal(f[2].FullBytes, &digAlg); err != nil {
		return nil, err
	}
	h, err := hashFor(digAlg.Algorithm)
	if err != nil {
		return nil, err
	}
	sp, err := readSignature(f, digAlg.Algorithm)
	if err != nil {
		return nil, err
	}
	signed, err := signedBytes(sp.attrs, h, content)
	if err != nil {
		return nil, err
	}
	if err := signer.CheckSignature(sp.alg, signed, sp.sig); err != nil {
		return nil, fmt.Errorf("TSA signature invalid: %w", err)
	}
	return signer, nil
}

// findSigner returns the certificate a SignerInfo names.
func findSigner(sid asn1.RawValue, certs []*x509.Certificate) (*x509.Certificate, error) {
	signer, err := matchSigner(sid, certs)
	if err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, errors.New("signing certificate not included in token (request certReq=true)")
	}
	return signer, nil
}

// matchSigner looks up the signer by issuer and serial, or by subject key id.
// The last matching certificate wins; nil means none matched.
func matchSigner(sid asn1.RawValue, certs []*x509.Certificate) (*x509.Certificate, error) {
	if sid.Class != asn1.ClassUniversal { // [0] SubjectKeyIdentifier
		return lastMatch(certs, func(c *x509.Certificate) bool { return bytes.Equal(c.SubjectKeyId, sid.Bytes) }), nil
	}
	var ias issuerAndSerial
	if _, err := asn1.Unmarshal(sid.FullBytes, &ias); err != nil {
		return nil, err
	}
	return lastMatch(certs, func(c *x509.Certificate) bool {
		return bytes.Equal(c.RawIssuer, ias.Issuer.FullBytes) && c.SerialNumber.Cmp(ias.Serial) == 0
	}), nil
}

func lastMatch(certs []*x509.Certificate, match func(*x509.Certificate) bool) *x509.Certificate {
	var found *x509.Certificate
	for _, c := range certs {
		if match(c) {
			found = c
		}
	}
	return found
}

// signatureParts is the part of a SignerInfo after the digest algorithm.
type signatureParts struct {
	attrs *asn1.RawValue // [0] signedAttrs; nil when the SignerInfo has none
	alg   x509.SignatureAlgorithm
	sig   []byte
}

// readSignature reads the optional signedAttrs, the signature algorithm and
// the signature from SignerInfo fields f (at least five).
func readSignature(f []asn1.RawValue, digest asn1.ObjectIdentifier) (signatureParts, error) {
	var sp signatureParts
	idx := 3
	if f[3].Class == asn1.ClassContextSpecific && f[3].Tag == 0 {
		sp.attrs = &f[3]
		idx = 4
	}
	if len(f) < idx+2 {
		return sp, errors.New("malformed SignerInfo")
	}
	var sAlg pkix.AlgorithmIdentifier
	if _, err := asn1.Unmarshal(f[idx].FullBytes, &sAlg); err != nil {
		return sp, err
	}
	if _, err := asn1.Unmarshal(f[idx+1].FullBytes, &sp.sig); err != nil {
		return sp, err
	}
	var err error
	sp.alg, err = sigAlg(digest, sAlg.Algorithm)
	return sp, err
}

// signedBytes returns what the signature covers: the DER SET of signedAttrs
// once its messageDigest and content-type check out, or the content itself
// when there are no signedAttrs.
func signedBytes(attrs *asn1.RawValue, h crypto.Hash, content []byte) ([]byte, error) {
	if attrs == nil {
		return content, nil
	}
	hh := h.New()
	hh.Write(content)
	if err := checkSignedAttrs(attrs.Bytes, hh.Sum(nil)); err != nil {
		return nil, err
	}
	return append([]byte{0x31}, attrs.FullBytes[1:]...), nil // [0] IMPLICIT -> SET
}

func checkSignedAttrs(contents, contentDigest []byte) error {
	attrs, err := elements(contents)
	if err != nil {
		return err
	}
	md, ctOK, err := readSignedAttrs(attrs)
	if err != nil {
		return err
	}
	if !bytes.Equal(md, contentDigest) {
		return errors.New("CMS messageDigest does not match TSTInfo")
	}
	if !ctOK {
		return errors.New("CMS content-type attribute is not TSTInfo")
	}
	return nil
}

// readSignedAttrs returns the messageDigest attribute and whether the content-type
// attribute names TSTInfo.
func readSignedAttrs(attrs []asn1.RawValue) (md []byte, ctOK bool, err error) {
	for _, a := range attrs {
		var attr struct {
			Type   asn1.ObjectIdentifier
			Values asn1.RawValue `asn1:"set"`
		}
		if _, err := asn1.Unmarshal(a.FullBytes, &attr); err != nil {
			return nil, false, err
		}
		switch {
		case attr.Type.Equal(oidMessageDigest):
			if _, err := asn1.Unmarshal(attr.Values.Bytes, &md); err != nil {
				return nil, false, fmt.Errorf("CMS messageDigest: %w", err)
			}
		case attr.Type.Equal(oidContentType):
			var v asn1.ObjectIdentifier
			if _, err := asn1.Unmarshal(attr.Values.Bytes, &v); err != nil {
				return nil, false, fmt.Errorf("CMS content-type: %w", err)
			}
			ctOK = v.Equal(oidTSTInfo)
		}
	}
	return md, ctOK, nil
}
