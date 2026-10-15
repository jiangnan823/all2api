//go:build qoder || full || (!qoder && !trae && !workbuddy)

// protocol.go implements the Qoder COSY wire primitives, ported from the
// verified reference implementation (vhub/qauth):
//   - the gateway's custom "Encode" layer (standard base64, 3-char blocks
//     rotated last-third-first, remapped onto a custom alphabet, '=' → '$'),
//   - the MD5 request signature over appCode + secret + date,
//   - the RSA+AES bearer envelope ("Bearer COSY.<payloadB64>.<sig>") and
//   - the two header families: machine-signature auth calls (exchange,
//     user/status) vs bearer session calls (chat, model list).
//
// Every constant here is protocol-mandated wire data observed on the real
// deployment, not a local choice — changing one breaks authentication. IDs
// are generated with crypto/rand (the reference uses github.com/google/uuid;
// this package is stdlib-only).
package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	cosyMD5 "crypto/md5" // #nosec G505 -- the Qoder COSY protocol mandates MD5 for its wire signatures; changing the hash breaks authentication.
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// --- Gateway "Encode" layer (custom base64 variant) ---

const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
)

var (
	s2c, c2s     [128]byte
	s2cOK, c2sOK [128]bool
)

func init() {
	for i := 0; i < 64; i++ {
		s := stdAlphabet[i]
		c := customAlphabet[i]
		s2c[s] = c
		s2cOK[s] = true
		c2s[c] = s
		c2sOK[c] = true
	}
	s2c['='] = '$'
	s2cOK['='] = true
	c2s['$'] = '='
	c2sOK['$'] = true
}

// customEncode implements the gateway's Encode: standard base64 whose n/3
// leading blocks are moved to the end (block rotation) and whose characters
// are remapped onto the custom alphabet. Padding '=' becomes '$'.
func customEncode(plaintext []byte) (string, error) {
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3
	rearranged := make([]byte, n)
	copy(rearranged[:a], std[n-a:])
	copy(rearranged[a:n-a], std[a:n-a])
	copy(rearranged[n-a:], std[:a])
	out := make([]byte, n)
	for i, ch := range rearranged {
		if ch >= 128 || !s2cOK[ch] {
			return "", fmt.Errorf("qoder: encode: char out of alphabet: %q", ch)
		}
		out[i] = s2c[ch]
	}
	return string(out), nil
}

// customDecode is the exact inverse of customEncode.
func customDecode(encoded string) ([]byte, error) {
	n := len(encoded)
	mapped := make([]byte, n)
	for i := 0; i < n; i++ {
		ch := encoded[i]
		if ch >= 128 || !c2sOK[ch] {
			return nil, fmt.Errorf("qoder: decode: char out of custom alphabet: %q", ch)
		}
		mapped[i] = c2s[ch]
	}
	a := n / 3
	std := make([]byte, n)
	copy(std[:a], mapped[n-a:])
	copy(std[a:n-a], mapped[a:n-a])
	copy(std[n-a:], mapped[:a])
	return base64.StdEncoding.DecodeString(string(std))
}

// --- Request signing ---

const (
	cosyAppCode = "cosy"
	cosyVersion = "0.1.43"
	// Protocol-mandated COSY signature secret shipped inside every official
	// client; it is wire-constant data, not a deployed credential.
	defaultSecret = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" // #nosec G101 -- well-known client-side protocol constant, not a secret credential; gitleaks:allow (repo-scoped allowlist in .gitleaks.toml)
)

// signatureSecret mirrors the reference's QODER_SIGNATURE_SECRET override;
// the default is the protocol value every official client ships.
func signatureSecret() string {
	if v := os.Getenv("QODER_SIGNATURE_SECRET"); v != "" {
		return v
	}
	return defaultSecret
}

// cosySign is the machine-identity signature carried in the date/signature
// header pair of the auth endpoint family (no session involved).
func cosySign(date string) string {
	// #nosec G401 -- protocol-mandated MD5 wire signature; changing it breaks authentication.
	sum := cosyMD5.Sum([]byte(cosyAppCode + "&" + signatureSecret() + "&" + date))
	return fmt.Sprintf("%x", sum)
}

// md5Hex signs the bearer envelope payload.
func md5Hex(s string) string {
	// #nosec G401 -- protocol-mandated MD5 wire signature; changing it breaks authentication.
	sum := cosyMD5.Sum([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// httpDate renders the auth header family's date value (RFC 1123, GMT).
func httpDate() string {
	return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

// --- Bearer envelope construction ---

// serverPubKeyPEM is the gateway's session-key wrapping public key (1024-bit
// RSA, protocol-fixed). The client encrypts a fresh 16-byte AES key with it;
// the gateway alone can unwrap the identity payload.
const serverPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var cosyRSAPubKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(serverPubKeyPEM))
	if block == nil {
		panic("qoder: failed to parse server public key PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic(fmt.Sprintf("qoder: failed to parse server public key: %v", err))
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		panic("qoder: server public key is not RSA")
	}
	cosyRSAPubKey = rsaPub
}

// authIdentity is the identity payload AES-sealed into every bearer envelope.
// Field order matches the official client's serialization.
type authIdentity struct {
	Name               string `json:"name"`
	Aid                string `json:"aid"`
	UID                string `json:"uid"`
	YxUID              string `json:"yx_uid"`
	OrganizationID     string `json:"organization_id"`
	OrganizationName   string `json:"organization_name"`
	UserType           string `json:"user_type"`
	SecurityOauthToken string `json:"security_oauth_token"`
	RefreshToken       string `json:"refresh_token"`
}

// session bundles everything one chat request needs to sign itself.
type session struct {
	machineID, machineToken, machineType string

	tempKey []byte // 16 raw bytes; AES key for the identity payload
	cosyKey string // base64(RSA(tempKey)); echoed in the cosy-key header
	info    string // base64(AES(authIdentity)); sealed into the bearer payload

	uid           string
	userType      string
	securityOauth string // the jt- jobToken; OpenAPI bearer (quota usage)
}

// newSession seals an identity into a fresh COSY session. machineID /
// machineToken / machineType are stable per account (generated once when the
// account's state is created); the temp key and request ids rotate per build.
func newSession(identity authIdentity, machineID, machineToken, machineType string) (*session, error) {
	tempKey := []byte(newUUIDString()[:16])
	//nolint:staticcheck // SA1019 -- the gateway unwraps the session key with PKCS #1 v1.5; OAEP would break authentication.
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, cosyRSAPubKey, tempKey)
	if err != nil {
		return nil, fmt.Errorf("qoder: wrap session key: %w", err)
	}
	plain, err := marshalNoEscape(identity)
	if err != nil {
		return nil, err
	}
	sealed, err := aesEncrypt(plain, tempKey)
	if err != nil {
		return nil, err
	}
	return &session{
		machineID:     machineID,
		machineToken:  machineToken,
		machineType:   machineType,
		tempKey:       tempKey,
		cosyKey:       base64.StdEncoding.EncodeToString(wrapped),
		info:          base64.StdEncoding.EncodeToString(sealed),
		uid:           identity.UID,
		userType:      identity.UserType,
		securityOauth: identity.SecurityOauthToken,
	}, nil
}

// aesEncrypt is AES-128-CBC with PKCS#7 padding, IV = key (16 bytes), exactly
// as the reference (and the official client) seals the identity payload.
func aesEncrypt(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padLen := 16 - len(plain)%16
	padded := make([]byte, len(plain)+padLen)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(encrypted, padded)
	return encrypted, nil
}

// payloadB64 builds the bearer envelope's middle segment:
// base64({"cosyVersion","ideVersion","info","requestId","version":"v1"}).
func (s *session) payloadB64() string {
	p := struct {
		CosyVersion string `json:"cosyVersion"`
		IdeVersion  string `json:"ideVersion"`
		Info        string `json:"info"`
		RequestID   string `json:"requestId"`
		Version     string `json:"version"`
	}{CosyVersion: cosyVersion, Info: s.info, RequestID: newUUIDString(), Version: "v1"}
	b, err := marshalNoEscape(p)
	if err != nil {
		// Marshal of a fixed string-only struct cannot fail; an empty payload
		// yields a signature the gateway rejects (classified auth failure)
		// instead of crashing the send path.
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// buildBearer composes "Bearer COSY.<payloadB64>.<sig>" with
// sig = MD5(payloadB64 \n cosyKey \n date \n body \n sigPath) — the exact
// five-part concatenation the gateway validates.
func (s *session) buildBearer(date, encodedBody, pathSig string) string {
	payloadB64 := s.payloadB64()
	sig := md5Hex(payloadB64 + "\n" + s.cosyKey + "\n" + date + "\n" + encodedBody + "\n" + pathSig)
	return "Bearer COSY." + payloadB64 + "." + sig
}

// sigPath reduces a full URL to the path the signature covers: host stripped,
// a leading "/algo" removed.
func sigPath(fullURL string) string {
	u, err := url.Parse(fullURL)
	if err != nil {
		return fullURL
	}
	//nolint:gocritic // singleExitLexpr not at play; TrimPrefix is the idiomatic form staticcheck asks for (S1017).
	return strings.TrimPrefix(u.Path, "/algo")
}

// --- Header families ---

// authHeaders is the machine-signature family for the auth endpoint family
// (jobToken exchange, user/status): date + signature, no bearer.
func authHeaders(machineID, machineToken, machineType, date, sig string) http.Header {
	h := http.Header{}
	h.Set("cosy-machinetoken", machineToken)
	h.Set("cosy-machinetype", machineType)
	h.Set("login-version", "v2")
	h.Set("appcode", cosyAppCode)
	h.Set("accept", "application/json")
	h.Set("accept-encoding", "identity")
	h.Set("cosy-version", cosyVersion)
	h.Set("cosy-clienttype", "5")
	h.Set("date", date)
	h.Set("signature", sig)
	h.Set("content-type", "application/json")
	h.Set("cosy-machineid", machineID)
	h.Set("user-agent", "Go-http-client/2.0")
	return h
}

// chatHeaders is the bearer-session family for chat calls: cosy envelope
// headers plus the COSY bearer. accept is "application/json" for the sync
// aggregated shape or "text/event-stream" for streams.
func chatHeaders(sess *session, date, bearer, accept string) http.Header {
	h := http.Header{}
	h.Set("cosy-data-policy", "AGREE")
	h.Set("content-type", "application/json")
	h.Set("cosy-machinetype", sess.machineType)
	h.Set("cosy-clienttype", "5")
	h.Set("cosy-date", date)
	h.Set("cosy-user", sess.uid)
	h.Set("cosy-key", sess.cosyKey)
	h.Set("accept", accept)
	h.Set("authorization", bearer)
	h.Set("accept-encoding", "identity")
	h.Set("cosy-version", cosyVersion)
	h.Set("cosy-machineid", sess.machineID)
	h.Set("cosy-machinetoken", sess.machineToken)
	h.Set("login-version", "v2")
	h.Set("user-agent", "Go-http-client/2.0")
	return h
}

// --- Shared serialization and ID generation ---

// marshalNoEscape serializes v to compact JSON without HTML escaping, matching
// the reference client's json.dumps(v, separators=(",", ":")).
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// newUUIDString returns a random RFC 4122 version-4 UUID string. Stdlib
// replacement for the reference's github.com/google/uuid dependency.
func newUUIDString() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A broken OS entropy source would make machine identities
		// predictable; failing the send loudly is the only honest move.
		panic("qoder: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newMachineIdentity mints the per-account machine fingerprint triple. Sizes
// mirror the reference: uuid id, 50-char token, 18-char type.
func newMachineIdentity() (machineID, machineToken, machineType string) {
	machineID = newUUIDString()
	machineToken = (newUUIDString() + newUUIDString())[:50]
	machineType = newUUIDString()[:18]
	return machineID, machineToken, machineType
}

// randHex returns n random hex characters (chat completion ids).
func randHex(n int) string {
	raw := make([]byte, (n+1)/2)
	if _, err := rand.Read(raw); err != nil {
		panic("qoder: crypto/rand unavailable: " + err.Error())
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, n)
	for _, b := range raw {
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out[:n])
}
