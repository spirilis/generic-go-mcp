package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"
)

// id_token verification, by hand: the library takes no JOSE dependency. Only what OIDC
// sign-in needs is here: compact JWS, RS256 and ES256, keys from a JWKS. Anything else is
// refused, never guessed at.

// jwtLeeway is the clock skew tolerated on exp, iat and nbf.
const jwtLeeway = 60 * time.Second

// minRSABits is the smallest RSA key accepted for signatures.
const minRSABits = 2048

// errUnknownKID means no key in the set can check the token: the IdP may have rotated its
// keys, so the caller refreshes them (rate-limited) and tries once more.
var errUnknownKID = errors.New("id_token signed with a key not in the provider's key set")

// jwk is one verification key from a JWKS.
type jwk struct {
	kid string
	alg string           // the key's own "alg", if it names one
	key crypto.PublicKey // *rsa.PublicKey or *ecdsa.PublicKey (P-256)
}

// parseJWKS reads the signing keys of a JWKS document. Keys this package cannot use (other key
// types or curves, encryption keys) are skipped, since IdPs publish them alongside; a set with
// no usable key at all is an error. An RSA key under 2048 bits is refused outright.
func parseJWKS(data []byte) ([]jwk, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	var keys []jwk
	for i, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			n, err1 := base64.RawURLEncoding.DecodeString(k.N)
			e, err2 := base64.RawURLEncoding.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
				return nil, fmt.Errorf("jwks: key %d (%q): malformed RSA n or e", i, k.Kid)
			}
			pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
			if pub.N.BitLen() < minRSABits {
				return nil, fmt.Errorf("jwks: key %d (%q): RSA key of %d bits is below %d", i, k.Kid, pub.N.BitLen(), minRSABits)
			}
			if pub.E < 3 || pub.E%2 == 0 {
				return nil, fmt.Errorf("jwks: key %d (%q): bad RSA exponent", i, k.Kid)
			}
			keys = append(keys, jwk{kid: k.Kid, alg: k.Alg, key: pub})
		case "EC":
			if k.Crv != "P-256" {
				continue
			}
			x, err1 := base64.RawURLEncoding.DecodeString(k.X)
			y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
			if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
				return nil, fmt.Errorf("jwks: key %d (%q): malformed P-256 x or y", i, k.Kid)
			}
			// crypto/ecdh refuses a point that is not on the curve; ecdsa.Verify would
			// otherwise be handed an invalid key.
			if _, err := ecdh.P256().NewPublicKey(slices.Concat([]byte{4}, x, y)); err != nil {
				return nil, fmt.Errorf("jwks: key %d (%q): %w", i, k.Kid, err)
			}
			pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			keys = append(keys, jwk{kid: k.Kid, alg: k.Alg, key: pub})
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks: no RS256 or ES256 signing key")
	}
	return keys, nil
}

// idTokenCheck is what an id_token must satisfy.
type idTokenCheck struct {
	issuer    string   // iss must equal it exactly
	clientID  string   // azp, when present, must equal it
	audiences []string // aud must contain one of them (the client id, and the configured audience)
	nonce     string   // must equal the token's nonce
	algs      []string // accepted header algorithms
	now       time.Time
}

// verifyIDToken checks an id_token's signature against keys and its claims against c, and
// returns its claims. JSON numbers are kept as json.Number.
func verifyIDToken(raw string, keys []jwk, c idTokenCheck) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("id_token is not a compact JWS")
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return nil, fmt.Errorf("id_token header: %w", err)
	}
	// The algorithm is checked against the allowlist before anything else: "none" and the HS*
	// family (an HMAC keyed with a public key the attacker also has) must never get as far as
	// a verification attempt.
	if !slices.Contains(c.algs, header.Alg) {
		return nil, fmt.Errorf("id_token algorithm %q is not accepted", header.Alg)
	}
	if len(header.Crit) > 0 {
		return nil, fmt.Errorf("id_token has critical header extensions %v", header.Crit)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("id_token signature is not base64url")
	}
	signed := []byte(parts[0] + "." + parts[1])

	verified, tried := false, false
	for _, k := range keys {
		if (header.Kid != "" && k.kid != header.Kid) || (k.alg != "" && k.alg != header.Alg) {
			continue
		}
		ok, usable := verifySignature(header.Alg, k.key, signed, sig)
		tried = tried || usable
		if ok {
			verified = true
			break
		}
	}
	switch {
	case !tried:
		return nil, errUnknownKID
	case !verified:
		return nil, errors.New("id_token signature does not verify")
	}

	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(decodeOrNil(parts[1])))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil || claims == nil {
		return nil, errors.New("id_token payload is not a JSON object")
	}
	if err := checkClaims(claims, c); err != nil {
		return nil, err
	}
	return claims, nil
}

// verifySignature reports whether sig is alg's signature of signed under key, and whether key
// is of the kind alg uses at all (a key of the wrong type was not "tried").
func verifySignature(alg string, key crypto.PublicKey, signed, sig []byte) (ok, usable bool) {
	digest := sha256.Sum256(signed)
	switch alg {
	case "RS256":
		pub, isRSA := key.(*rsa.PublicKey)
		if !isRSA {
			return false, false
		}
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) == nil, true
	case "ES256":
		pub, isEC := key.(*ecdsa.PublicKey)
		if !isEC || pub.Curve != elliptic.P256() {
			return false, false
		}
		// JWS carries the raw r||s, 32 bytes each, not ASN.1.
		if len(sig) != 64 {
			return false, true
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(pub, digest[:], r, s), true
	}
	return false, false
}

func checkClaims(claims map[string]any, c idTokenCheck) error {
	if iss, _ := claims["iss"].(string); iss != c.issuer {
		return fmt.Errorf("id_token issuer %q is not %q", iss, c.issuer)
	}
	if sub, _ := claims["sub"].(string); sub == "" {
		return errors.New("id_token has no sub")
	}

	var aud []string
	switch v := claims["aud"].(type) {
	case string:
		aud = []string{v}
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok {
				aud = append(aud, s)
			}
		}
	}
	if !slices.ContainsFunc(aud, func(a string) bool { return a != "" && slices.Contains(c.audiences, a) }) {
		return fmt.Errorf("id_token audience %v does not include this client", aud)
	}
	// OIDC Core 3.1.3.7: with several audiences the authorized party must be this client, so a
	// token issued to another client that merely lists this one is not accepted as a login.
	azp, hasAZP := claims["azp"].(string)
	if (len(aud) > 1 || hasAZP) && azp != c.clientID {
		return fmt.Errorf("id_token authorized party %q is not this client", azp)
	}

	exp, ok := numericDate(claims["exp"])
	if !ok {
		return errors.New("id_token has no exp")
	}
	if c.now.After(exp.Add(jwtLeeway)) {
		return errors.New("id_token has expired")
	}
	iat, ok := numericDate(claims["iat"])
	if !ok {
		return errors.New("id_token has no iat")
	}
	if iat.After(c.now.Add(jwtLeeway)) {
		return errors.New("id_token was issued in the future")
	}
	if nbf, present := numericDate(claims["nbf"]); present && nbf.After(c.now.Add(jwtLeeway)) {
		return errors.New("id_token is not valid yet")
	}

	if nonce, _ := claims["nonce"].(string); c.nonce == "" || nonce != c.nonce {
		return errors.New("id_token nonce does not match this sign-in")
	}
	return nil
}

// numericDate reads a JWT NumericDate (seconds since the epoch, possibly fractional).
func numericDate(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f < 0 || f > 1e12 { // past year 33000: garbage, and would overflow below
		return time.Time{}, false
	}
	sec := math.Floor(f)
	return time.Unix(int64(sec), int64((f-sec)*1e9)), true
}

func decodeSegment(seg string, v any) error {
	data, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("not base64url")
	}
	return json.Unmarshal(data, v)
}

// decodeOrNil decodes a base64url segment, returning nil (which fails JSON decoding) when it
// is malformed.
func decodeOrNil(seg string) []byte {
	data, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return nil
	}
	return data
}
