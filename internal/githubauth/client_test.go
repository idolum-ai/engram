package githubauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClientMintsExactlyScopedInstallationToken(t *testing.T) {
	privateKeyPEM, privateKey := testPrivateKey(t)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	var installationGets, tokenPosts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyTestJWT(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), privateKey, 123, now)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations/456":
			installationGets++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 456, "app_slug": "idolum-app",
				"account":     map[string]any{"login": "idolum-ai"},
				"permissions": map[string]string{"contents": "write", "pull_requests": "write"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/456/access_tokens":
			tokenPosts++
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Repositories) != 1 || body.Repositories[0] != "engram" ||
				body.Permissions["contents"] != "read" || body.Permissions["pull_requests"] != "write" {
				t.Fatalf("token request was not exact: %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_new_format.jwt.payload",
				"expires_at": now.Add(time.Hour),
				"permissions": map[string]string{
					"contents": "read", "pull_requests": "write", "metadata": "read",
				},
				"repositories": []map[string]string{{"full_name": "idolum-ai/engram"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient()
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	client.Now = func() time.Time { return now }
	token, err := client.Mint(context.Background(), App{AppID: 123, InstallationID: 456}, privateKeyPEM,
		[]string{"idolum-ai/engram"}, map[string]string{"contents": "read", "pull_requests": "write"})
	if err != nil {
		t.Fatal(err)
	}
	if token.Value != "ghs_new_format.jwt.payload" || installationGets != 1 || tokenPosts != 1 {
		t.Fatalf("unexpected token flow: token=%q gets=%d posts=%d", token.Value, installationGets, tokenPosts)
	}
}

func TestValidateTokenFailsClosedOnExtraAuthority(t *testing.T) {
	now := time.Now().UTC()
	token := Token{
		Value:       "secret",
		ExpiresAt:   now.Add(time.Hour),
		Permissions: map[string]string{"contents": "read", "issues": "write"},
	}
	token.Repositories = append(token.Repositories, struct {
		FullName string `json:"full_name"`
	}{FullName: "idolum-ai/engram"})
	if err := ValidateToken(token, []string{"idolum-ai/engram"}, map[string]string{"contents": "read"}, now); err == nil {
		t.Fatal("unrequested write permission was accepted")
	}
	token.Permissions = map[string]string{"contents": "read"}
	token.Repositories[0].FullName = "idolum-ai/other"
	if err := ValidateToken(token, []string{"idolum-ai/engram"}, map[string]string{"contents": "read"}, now); err == nil {
		t.Fatal("unexpected repository was accepted")
	}
}

func verifyTestJWT(t *testing.T, token string, key *rsa.PrivateKey, appID int64, now time.Time) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("JWT signature: %v", err)
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != strconv.FormatInt(appID, 10) || claims.Iat != now.Add(-time.Minute).Unix() || claims.Exp != now.Add(9*time.Minute).Unix() {
		t.Fatalf("unexpected JWT claims: %#v", claims)
	}
}
