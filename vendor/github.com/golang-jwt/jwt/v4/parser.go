package jwt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type Parser struct {
	// If populated, only these methods will be considered valid.
	//
	// Deprecated: In future releases, this field will not be exported anymore and should be set with an option to NewParser instead.
	ValidMethods []string

	// Use JSON Number format in JSON decoder.
	//
	// Deprecated: In future releases, this field will not be exported anymore and should be set with an option to NewParser instead.
	UseJSONNumber bool

	// Skip claims validation during token parsing.
	//
	// Deprecated: In future releases, this field will not be exported anymore and should be set with an option to NewParser instead.
	SkipClaimsValidation bool
}

// NewParser creates a new Parser with the specified options
func NewParser(options ...ParserOption) *Parser {
	p := &Parser{}

	// loop through our parsing options and apply them
	for _, option := range options {
		option(p)
	}

	return p
}

// Parse parses, validates, verifies the signature and returns the parsed token. keyFunc will
// receive the parsed token and should return the key for validating.
func (p *Parser) Parse(tokenString string, keyFunc Keyfunc) (*Token, error) {
	return p.ParseWithClaims(tokenString, MapClaims{}, keyFunc)
}

// ParseWithClaims parses, validates, and verifies like Parse, but supplies a default object
// implementing the Claims interface. This provides default values which can be overridden and
// allows a caller to use their own type, rather than the default MapClaims implementation of
// Claims.
//
// Note: If you provide a custom claim implementation that embeds one of the standard claims (such
// as RegisteredClaims), make sure that a) you either embed a non-pointer version of the claims or
// b) if you are using a pointer, allocate the proper memory for it before passing in the overall
// claims, otherwise you might run into a panic.
func (p *Parser) ParseWithClaims(tokenString string, claims Claims, keyFunc Keyfunc) (*Token, error) {
	token, parts, err := p.ParseUnverified(tokenString, claims)
	if err != nil {
		return token, err
	}

	// Verify signing method is in the required set
	if p.ValidMethods != nil {
		var signingMethodValid = false
		var alg = token.Method.Alg()
		for _, m := range p.ValidMethods {
			if m == alg {
				signingMethodValid = true
				break
			}
		}
		if !signingMethodValid {
			// signing method is not in the listed set
			return token, NewValidationError(fmt.Sprintf("signing method %v is invalid", alg), ValidationErrorSignatureInvalid)
		}
	}

	// Lookup key
	var key interface{}
	if keyFunc == nil {
		// keyFunc was not provided.  short circuiting validation
		return token, NewValidationError("no Keyfunc was provided.", ValidationErrorUnverifiable)
	}
	if key, err = keyFunc(token); err != nil {
		// keyFunc returned an error
		if ve, ok := err.(*ValidationError); ok {
			return token, ve
		}
		return token, &ValidationError{Inner: err, Errors: ValidationErrorUnverifiable}
	}

	// Perform validation
	token.Signature = parts[2]
	if err := token.Method.Verify(strings.Join(parts[0:2], "."), token.Signature, key); err != nil {
		return token, &ValidationError{Inner: err, Errors: ValidationErrorSignatureInvalid}
	}

	vErr := &ValidationError{}

	// Validate Claims
	if !p.SkipClaimsValidation {
		if err := token.Claims.Valid(); err != nil {
			// If the Claims Valid returned an error, check if it is a validation error,
			// If it was another error type, create a ValidationError with a generic ClaimsInvalid flag set
			if e, ok := err.(*ValidationError); !ok {
				vErr = &ValidationError{Inner: err, Errors: ValidationErrorClaimsInvalid}
			} else {
				vErr = e
			}
			return token, vErr
		}
	}

	// No errors so far, token is valid.
	token.Valid = true

	return token, nil
}

// ParseUnverified parses the token but doesn't validate the signature.
//
// WARNING: Don't use this method unless you know what you're doing.
//
// It's only ever useful in cases where you know the signature is valid (because it has
// been checked previously in the stack) and you want to extract values from it.
func (p *Parser) ParseUnverified(tokenString string, claims Claims) (token *Token, parts []string, err error) {
	parts = strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, parts, NewValidationError("token contains an invalid number of segments", ValidationErrorMalformed)
	}

	token = &Token{Raw: tokenString}

	// parse Header
	var headerBytes []byte
	if headerBytes, err = DecodeSegment(parts[0]); err != nil {
		if strings.HasPrefix(strings.ToLower(tokenString), "bearer ") {
			return token, parts, NewValidationError("tokenstring should not contain 'bearer '", ValidationErrorMalformed)
		}
		return token, parts, &ValidationError{Inner: err, Errors: ValidationErrorMalformed}
	}
	if err = json.Unmarshal(headerBytes, &token.Header); err != nil {
		return token, parts, &ValidationError{Inner: err, Errors: ValidationErrorMalformed}
	}

	// parse Claims
	var claimBytes []byte
	token.Claims = claims

	if claimBytes, err = DecodeSegment(parts[1]); err != nil {
		return token, parts, &ValidationError{Inner: err, Errors: ValidationErrorMalformed}
	}
	dec := json.NewDecoder(bytes.NewBuffer(claimBytes))
	if p.UseJSONNumber {
		dec.UseNumber()
	}
	// JSON Decode.  Special case for map type to avoid weird pointer behavior
	if c, ok := token.Claims.(MapClaims); ok {
		err = dec.Decode(&c)
	} else {
		err = dec.Decode(&claims)
	}
	// Handle decode error
	if err != nil {
		return token, parts, &ValidationError{Inner: err, Errors: ValidationErrorMalformed}
	}

	// Lookup signature method
	if method, ok := token.Header["alg"].(string); ok {
		if token.Method = GetSigningMethod(method); token.Method == nil {
			return token, parts, NewValidationError("signing method (alg) is unavailable.", ValidationErrorUnverifiable)
		}
	} else {
		return token, parts, NewValidationError("signing method (alg) is unspecified.", ValidationErrorUnverifiable)
	}

	return token, parts, nil
}
