package oauth1

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type providerRequest struct {
	req               *http.Request
	oauthParams       map[string]string
	signatureToVerify string
	signatureMethod   string
	timestamp         int64
	clientKey         string
	nonce             string
}

// ClientStorage represents an OAuth 1 provider's database of clients.
type ClientStorage = interface {
	// GetSigner returns the signer that should be used to validate the signature for a client.
	// To avoid timing attacks, GetSigner should return a Signer and a non-nil error
	// if the clientKey is invalid. ValidateRequest will still compute a signature
	// so that the runtime of ValidateRequest is about the same regardless of the clientKey's validity.
	// The http request is also available for additional validation, e.g. checking for HTTPS.
	GetSigner(ctx context.Context, clientKey, signatureMethod string, req *http.Request) (Signer, error)

	// ValidateNonce returns an error if a nonce has been used before.
	//
	// Per Section 3.3 of the spec:
	//    The timestamp value MUST be a positive integer.  Unless otherwise
	//    specified by the server's documentation, the timestamp is expressed
	//    in the number of seconds since January 1, 1970 00:00:00 GMT.
	//
	//    A nonce is a random string, uniquely generated by the client to allow
	//    the server to verify that a request has never been made before and
	//    helps prevent replay attacks when requests are made over a non-secure
	//    channel.  The nonce value MUST be unique across all requests with the
	//    same timestamp, client credentials, and token combinations.
	//
	//    To avoid the need to retain an infinite number of nonce values for
	//    future checks, servers MAY choose to restrict the time period after
	//    which a request with an old timestamp is rejected.  Note that this
	//    restriction implies a level of synchronization between the client's
	//    and server's clocks.
	ValidateNonce(ctx context.Context, clientKey, nonce string, timestamp int64, req *http.Request) error
}

var authorizationHeaderParamPattern = regexp.MustCompile(`^\s*([^=]+)="?(\S*?)"?\s*$`)

func newProviderRequest(req *http.Request) (*providerRequest, error) {
	authParams := make(map[string]string)
	authHeader := req.Header.Get(authorizationHeaderParam)
	if len(authHeader) > len(authorizationPrefix) {
		authHeaderPrefix := strings.ToLower(authHeader[:len(authorizationPrefix)])
		if authHeaderPrefix == strings.ToLower(authorizationPrefix) {
			authHeaderSuffix := authHeader[len(authorizationPrefix):]
			for _, pair := range strings.Split(authHeaderSuffix, ",") {
				if match := authorizationHeaderParamPattern.FindStringSubmatch(pair); match == nil {
					return nil, fmt.Errorf("Invalid Authorization header")
				} else if value, err := url.PathUnescape(match[2]); err == nil {
					authParams[match[1]] = value
				} else {
					return nil, err
				}
			}
		}
	}
	allParams, err := collectParameters(req, authParams)
	if err != nil {
		return nil, err
	}
	if err = checkMandatoryParams(allParams); err != nil {
		return nil, err
	}
	sig := allParams[oauthSignatureParam]
	delete(allParams, oauthSignatureParam)
	timestamp, err := strconv.ParseInt(allParams[oauthTimestampParam], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("unable to parse timestamp: %v", err)
	} else if timestamp <= 0 {
		return nil, fmt.Errorf("invalid timestamp %v", timestamp)
	}
	if version, ok := allParams[oauthVersionParam]; ok && version != defaultOauthVersion {
		return nil, fmt.Errorf("incorrect oauth version %v", version)
	}
	preq := &providerRequest{
		req:               req,
		oauthParams:       allParams,
		signatureToVerify: sig,
		signatureMethod:   allParams[oauthSignatureMethodParam],
		timestamp:         timestamp,
		clientKey:         allParams[oauthConsumerKeyParam],
		nonce:             allParams[oauthNonceParam],
	}
	return preq, nil
}

func checkMandatoryParams(params map[string]string) error {
	var missingParams []string
	for _, param := range []string{oauthSignatureParam, oauthConsumerKeyParam, oauthNonceParam, oauthTimestampParam, oauthSignatureMethodParam} {
		if _, ok := params[param]; !ok {
			missingParams = append(missingParams, param)
		}
	}
	if len(missingParams) > 0 {
		return fmt.Errorf("missing required oauth params %v", strings.Join(missingParams, ", "))
	}
	if _, hasAccessToken := params[oauthTokenParam]; hasAccessToken {
		return fmt.Errorf("token signature validation not implemented")
	}
	return nil
}

var errSignatureMismatch = fmt.Errorf("signature mismatch")

func (r providerRequest) checkSignature(signer Signer) error {
	if signer == nil {
		return errSignatureMismatch
	}
	base := signatureBase(r.req, r.oauthParams)
	signature, err := signer.Sign("", base)
	if err != nil {
		return err
	}

	// near constant time string comparison to avoid timing attacks
	// https://rdist.root.org/2010/01/07/timing-independent-array-comparison/
	sigToVerify := r.signatureToVerify
	if len(sigToVerify) != len(signature) {
		return errSignatureMismatch
	}
	result := byte(0)
	for i, r := range []byte(signature) {
		result |= r ^ sigToVerify[i]
	}
	if result != 0 {
		return errSignatureMismatch
	}
	return nil
}

// ValidateSignature checks that req contains a valid OAUTH 1 signature.
// It returns nil if the signature is valid, or an error if the validation fails.
func ValidateSignature(ctx context.Context, req *http.Request, v ClientStorage) error {
	preq, err := newProviderRequest(req)
	if err != nil {
		return err
	}
	if err = v.ValidateNonce(ctx, preq.clientKey, preq.nonce, preq.timestamp, req); err != nil {
		return err
	}
	signer, invalidClient := v.GetSigner(ctx, preq.clientKey, preq.signatureMethod, req)

	// Check signature even if client is invalid to prevent timing attacks.
	invalidSignature := preq.checkSignature(signer)
	if invalidClient != nil {
		return invalidClient
	}
	return invalidSignature
}
