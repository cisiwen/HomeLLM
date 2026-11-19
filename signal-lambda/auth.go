// Cognito JWT verification - a copy of lambda-videochat/auth.go (same reasoning as that
// Lambda's own main.go doc comment for duplicating rather than sharing a package across
// separately-deployed Lambdas), plus reading the token from the Authorization header first.
// Unlike video chat, HomeLLM has no browser client, so every caller can set headers - and a
// header keeps the token out of URLs, which end up in access logs. The idToken query
// parameter is still accepted as a fallback.
package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

var (
	cognitoUserPoolID = os.Getenv("COGNITO_USER_POOL_ID")
	cognitoClientID   = os.Getenv("COGNITO_CLIENT_ID")
	cognitoRegion     = os.Getenv("COGNITO_REGION")

	// Scripted testing only - never set from a literal in source, only ever via
	// `HOMELLM_TEST_BYPASS_SECRET=... cdk deploy HomeLlmStack`.
	testBypassSecret = os.Getenv("TEST_BYPASS_SECRET")

	jwks keyfunc.Keyfunc
)

const testBypassSubject = "test-bypass-user"

func issuerURL() string {
	return fmt.Sprintf("https://cognito-idp.%s.amazonaws.com/%s", cognitoRegion, cognitoUserPoolID)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header value.
func bearerToken(header string) string {
	if t, ok := strings.CutPrefix(header, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// authenticate verifies a Cognito ID token and returns its subject (the user ID).
func authenticate(idToken string) (subject string, err error) {
	if testBypassSecret != "" && subtle.ConstantTimeCompare([]byte(idToken), []byte(testBypassSecret)) == 1 {
		log.Printf("auth: connection authenticated via TEST_BYPASS_SECRET, subject=%s", testBypassSubject)
		return testBypassSubject, nil
	}
	if cognitoUserPoolID == "" || cognitoClientID == "" {
		return "", fmt.Errorf("auth not configured")
	}
	if idToken == "" {
		return "", fmt.Errorf("missing idToken")
	}
	if jwks == nil {
		jwks, err = keyfunc.NewDefault([]string{issuerURL() + "/.well-known/jwks.json"})
		if err != nil {
			return "", fmt.Errorf("failed to fetch JWKS: %w", err)
		}
	}
	token, err := jwt.Parse(idToken, jwks.Keyfunc,
		jwt.WithIssuer(issuerURL()),
		jwt.WithAudience(cognitoClientID),
		jwt.WithValidMethods([]string{"RS256"}),
	)
	if err != nil || !token.Valid {
		return "", fmt.Errorf("invalid token: %w", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("unexpected claims type")
	}
	if tokenUse, _ := claims["token_use"].(string); tokenUse != "id" {
		return "", fmt.Errorf("not an ID token")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", fmt.Errorf("missing subject claim")
	}
	return sub, nil
}
