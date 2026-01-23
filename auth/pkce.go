package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
)

// PKCE validation constants
const (
	PKCEMethodS256  = "S256"
	PKCEMethodPlain = "plain" // NOT recommended but allowed per spec

	// code_verifier must be 43-128 characters, unreserved chars only
	PKCEVerifierMinLength = 43
	PKCEVerifierMaxLength = 128
)

var pkceVerifierPattern = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)

// ValidatePKCE validates the code_verifier against the stored code_challenge
func ValidatePKCE(codeVerifier, codeChallenge, method string) error {
	if codeVerifier == "" {
		return fmt.Errorf("code_verifier is required (PKCE is mandatory)")
	}

	if !pkceVerifierPattern.MatchString(codeVerifier) {
		return fmt.Errorf("invalid code_verifier format")
	}

	var computed string
	switch method {
	case PKCEMethodS256, "": // Default to S256 if not specified
		hash := sha256.Sum256([]byte(codeVerifier))
		computed = base64.RawURLEncoding.EncodeToString(hash[:])
	case PKCEMethodPlain:
		computed = codeVerifier
	default:
		return fmt.Errorf("unsupported code_challenge_method: %s", method)
	}

	if computed != codeChallenge {
		return fmt.Errorf("code_verifier does not match code_challenge")
	}

	return nil
}

// ValidateCodeChallenge validates a code_challenge during authorization
func ValidateCodeChallenge(codeChallenge, method string) error {
	if codeChallenge == "" {
		return fmt.Errorf("code_challenge is required (PKCE is mandatory)")
	}

	// S256 produces 43 character base64url output
	if method == PKCEMethodS256 || method == "" {
		if len(codeChallenge) != 43 {
			return fmt.Errorf("invalid code_challenge length for S256")
		}
	}

	return nil
}
