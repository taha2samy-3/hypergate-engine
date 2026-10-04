package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TokenAudience is the audience of the projected ServiceAccount token engines send.
const TokenAudience = "hypergate-identity"

// TokenReviewAuthenticator validates engine tokens with the Kubernetes
// TokenReview API and accepts only the ServiceAccounts Allowed approves.
// Positive results are cached until the token expires (at most TTL).
type TokenReviewAuthenticator struct {
	Client   kubernetes.Interface
	Audience string
	// Allowed decides whether an authenticated username
	// (system:serviceaccount:<namespace>:<name>) may read identities.
	Allowed func(ctx context.Context, username string) bool
	// TTL bounds how long a positive result is cached. Default 5 minutes.
	TTL time.Duration

	now   func() time.Time
	mu    sync.Mutex
	cache map[[sha256.Size]byte]cachedReview
}

type cachedReview struct {
	username string
	expires  time.Time
}

// Authenticate implements Authenticator.
func (a *TokenReviewAuthenticator) Authenticate(ctx context.Context, token string) (string, error) {
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	key := sha256.Sum256([]byte(token))

	a.mu.Lock()
	if c, ok := a.cache[key]; ok && now().Before(c.expires) {
		a.mu.Unlock()
		// Re-check authorization: an engine namespace may have been removed.
		if !a.Allowed(ctx, c.username) {
			return "", fmt.Errorf("%w: %s", ErrForbidden, c.username)
		}
		return c.username, nil
	}
	a.mu.Unlock()

	audience := a.Audience
	if audience == "" {
		audience = TokenAudience
	}
	review, err := a.Client.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	st := review.Status
	if !st.Authenticated {
		msg := st.Error
		if msg == "" {
			msg = "not authenticated"
		}
		return "", fmt.Errorf("%w: %s", ErrUnauthenticated, msg)
	}
	if len(st.Audiences) > 0 && !slices.Contains(st.Audiences, audience) {
		return "", fmt.Errorf("%w: token audience is not %q", ErrUnauthenticated, audience)
	}
	username := st.User.Username
	if !a.Allowed(ctx, username) {
		return "", fmt.Errorf("%w: %s", ErrForbidden, username)
	}

	ttl := a.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	expires := now().Add(ttl)
	if exp, ok := tokenExpiry(token); ok && exp.Before(expires) {
		expires = exp
	}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[[sha256.Size]byte]cachedReview{}
	}
	if len(a.cache) > 1024 {
		for k, c := range a.cache {
			if !now().Before(c.expires) {
				delete(a.cache, k)
			}
		}
	}
	a.cache[key] = cachedReview{username: username, expires: expires}
	a.mu.Unlock()
	return username, nil
}

// tokenExpiry reads the exp claim of a JWT without verifying it. It only bounds
// the cache lifetime of a token the TokenReview API has already accepted.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// ServiceAccountUsername returns the username Kubernetes gives a ServiceAccount.
func ServiceAccountUsername(namespace, name string) string {
	return "system:serviceaccount:" + namespace + ":" + name
}
