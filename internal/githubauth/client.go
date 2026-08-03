package githubauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultGitHubAPIBase = "https://api.github.com"

type Installation struct {
	ID          int64             `json:"id"`
	AppSlug     string            `json:"app_slug"`
	Permissions map[string]string `json:"permissions"`
	Account     struct {
		Login string `json:"login"`
	} `json:"account"`
	SuspendedAt *time.Time `json:"suspended_at"`
}

type Token struct {
	Value        string            `json:"token"`
	ExpiresAt    time.Time         `json:"expires_at"`
	Permissions  map[string]string `json:"permissions"`
	Repositories []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

type Minter interface {
	InspectInstallation(context.Context, App, []byte) (Installation, error)
	Mint(context.Context, App, []byte, []string, map[string]string) (Token, error)
	Revoke(context.Context, string) error
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
}

func NewClient() *Client {
	return &Client{
		BaseURL: defaultGitHubAPIBase,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		Now: time.Now,
	}
}

func (c *Client) InspectInstallation(ctx context.Context, app App, privateKeyPEM []byte) (Installation, error) {
	installationID := app.EffectiveInstallationID()
	if installationID <= 0 {
		return Installation{}, fmt.Errorf("GitHub App request is not bound to an installation")
	}
	jwt, err := c.appJWT(app.AppID, privateKeyPEM)
	if err != nil {
		return Installation{}, err
	}
	var installation Installation
	path := fmt.Sprintf("/app/installations/%d", installationID)
	if err := c.request(ctx, http.MethodGet, path, jwt, nil, &installation); err != nil {
		return Installation{}, fmt.Errorf("inspect GitHub App installation %d: %w", installationID, err)
	}
	if installation.ID != installationID || strings.TrimSpace(installation.Account.Login) == "" {
		return Installation{}, fmt.Errorf("GitHub returned an unexpected installation identity")
	}
	if installation.SuspendedAt != nil {
		return Installation{}, fmt.Errorf("GitHub App installation is suspended")
	}
	return installation, nil
}

func (c *Client) Mint(ctx context.Context, app App, privateKeyPEM []byte, repositories []string, permissions map[string]string) (Token, error) {
	installationID := app.EffectiveInstallationID()
	if installationID <= 0 {
		return Token{}, fmt.Errorf("GitHub App request is not bound to an installation")
	}
	installation, err := c.InspectInstallation(ctx, app, privateKeyPEM)
	if err != nil {
		return Token{}, err
	}
	if err := ValidateInstallationScope(installation, repositories, permissions); err != nil {
		return Token{}, err
	}
	repositoryNames := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		parts := strings.Split(repository, "/")
		repositoryNames = append(repositoryNames, parts[1])
	}
	jwt, err := c.appJWT(app.AppID, privateKeyPEM)
	if err != nil {
		return Token{}, err
	}
	payload := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{
		Repositories: repositoryNames,
		Permissions:  permissions,
	}
	var token Token
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := c.request(ctx, http.MethodPost, path, jwt, payload, &token); err != nil {
		return Token{}, fmt.Errorf("GitHub rejected the token request for installation %d; verify that this installation alone covers every requested repository and permission: %w", installationID, err)
	}
	if err := ValidateToken(token, repositories, permissions, c.now()); err != nil {
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		revokeErr := c.Revoke(revokeCtx, token.Value)
		cancel()
		if revokeErr != nil {
			return Token{}, fmt.Errorf("%w; revoke rejected GitHub token: %v", err, revokeErr)
		}
		return Token{}, err
	}
	return token, nil
}

func ValidateInstallationScope(installation Installation, repositories []string, permissions map[string]string) error {
	if !PermissionsSubset(permissions, installation.Permissions) {
		return fmt.Errorf("requested permissions exceed the GitHub App installation")
	}
	for _, repository := range repositories {
		parts := strings.Split(repository, "/")
		if len(parts) != 2 || !strings.EqualFold(parts[0], installation.Account.Login) {
			return fmt.Errorf("repository %q does not belong to installation account %q", repository, installation.Account.Login)
		}
	}
	return nil
}

func (c *Client) Revoke(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	return c.request(ctx, http.MethodDelete, "/installation/token", token, nil, nil)
}

func ValidateToken(token Token, requestedRepositories []string, requestedPermissions map[string]string, now time.Time) error {
	if strings.TrimSpace(token.Value) == "" || !token.ExpiresAt.After(now) {
		return fmt.Errorf("GitHub returned an invalid installation token")
	}
	effectiveRepositories := make([]string, 0, len(token.Repositories))
	for _, repository := range token.Repositories {
		effectiveRepositories = append(effectiveRepositories, repository.FullName)
	}
	if !sameStringSet(effectiveRepositories, requestedRepositories) {
		return fmt.Errorf("GitHub token repository scope did not match the request")
	}
	for name, level := range requestedPermissions {
		if token.Permissions[name] != level {
			return fmt.Errorf("GitHub token permission scope did not match the request")
		}
	}
	for name, level := range token.Permissions {
		if requestedLevel, requested := requestedPermissions[name]; requested {
			if level != requestedLevel {
				return fmt.Errorf("GitHub token permission scope did not match the request")
			}
			continue
		}
		if name != "metadata" || level != "read" {
			return fmt.Errorf("GitHub token contained an unrequested permission")
		}
	}
	return nil
}

func (c *Client) appJWT(appID int64, privateKeyPEM []byte) (string, error) {
	key, err := ParsePrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	now := c.now().UTC()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (c *Client) request(ctx context.Context, method, path, bearer string, body, output any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	parsedBase, err := url.Parse(base)
	if err != nil || !parsedBase.IsAbs() || parsedBase.Host == "" || parsedBase.User != nil {
		return fmt.Errorf("invalid GitHub API base URL")
	}
	endpoint := parsedBase.ResolveReference(&url.URL{Path: path})
	var reader io.Reader
	if body != nil {
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return fmt.Errorf("encode GitHub request: %w", err)
		}
		reader = &encoded
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return fmt.Errorf("create GitHub request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("GitHub API request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("GitHub API request failed with HTTP %d", response.StatusCode)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode GitHub API response: %w", err)
	}
	return nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Slice(left, func(i, j int) bool { return strings.ToLower(left[i]) < strings.ToLower(left[j]) })
	sort.Slice(right, func(i, j int) bool { return strings.ToLower(right[i]) < strings.ToLower(right[j]) })
	for index := range left {
		if !strings.EqualFold(left[index], right[index]) {
			return false
		}
	}
	return true
}
