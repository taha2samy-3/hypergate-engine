package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const engineUser = "system:serviceaccount:hyper-system:hyper-engine-sa"

// reviewer answers TokenReviews: token -> username ("" = not authenticated).
func reviewer(t *testing.T, users map[string]string, audiences []string, calls *atomic.Int32) *fake.Clientset {
	t.Helper()
	c := fake.NewClientset()
	c.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		calls.Add(1)
		tr := action.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		if tr.Spec.Token == "boom" {
			return true, nil, errors.New("apiserver unavailable")
		}
		if got := tr.Spec.Audiences; len(got) != 1 || got[0] != TokenAudience {
			t.Errorf("TokenReview audiences = %v", got)
		}
		out := tr.DeepCopy()
		if u := users[tr.Spec.Token]; u != "" {
			out.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: u}, Audiences: audiences}
		} else {
			out.Status = authnv1.TokenReviewStatus{Error: "invalid bearer token"}
		}
		return true, out, nil
	})
	return c
}

func allowEngines(_ context.Context, user string) bool { return user == engineUser }

func jwtWithExp(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".sig"
}

func TestTokenReviewAuthenticator(t *testing.T) {
	var calls atomic.Int32
	good := jwtWithExp(time.Now().Add(time.Hour))
	a := &TokenReviewAuthenticator{
		Client:  reviewer(t, map[string]string{good: engineUser, "intruder": "system:serviceaccount:shop:default"}, []string{TokenAudience}, &calls),
		Allowed: allowEngines,
	}
	ctx := context.Background()

	if user, err := a.Authenticate(ctx, good); err != nil || user != engineUser {
		t.Fatalf("good token: %q %v", user, err)
	}
	if _, err := a.Authenticate(ctx, good); err != nil || calls.Load() != 1 {
		t.Fatalf("second call must hit the cache (calls=%d, err=%v)", calls.Load(), err)
	}
	if _, err := a.Authenticate(ctx, "bad"); !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "invalid bearer token") {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := a.Authenticate(ctx, "intruder"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other service account: %v", err)
	}
	if _, err := a.Authenticate(ctx, "boom"); err == nil || errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrForbidden) {
		t.Fatalf("API failure must be a plain error, got %v", err)
	}
	// Failed reviews are never cached.
	before := calls.Load()
	_, _ = a.Authenticate(ctx, "bad")
	if calls.Load() != before+1 {
		t.Fatal("negative result was cached")
	}
}

func TestTokenReviewAuthenticator_CacheBoundsAndRevocation(t *testing.T) {
	var calls atomic.Int32
	now := time.Now()
	soon := jwtWithExp(now.Add(time.Minute))
	allowed := true
	a := &TokenReviewAuthenticator{
		Client:  reviewer(t, map[string]string{soon: engineUser}, nil, &calls),
		Allowed: func(context.Context, string) bool { return allowed },
		TTL:     time.Hour,
		now:     func() time.Time { return now },
	}
	ctx := context.Background()
	if _, err := a.Authenticate(ctx, soon); err != nil {
		t.Fatal(err)
	}
	// The token expires before the TTL, so the cache entry does too.
	now = now.Add(2 * time.Minute)
	_, _ = a.Authenticate(ctx, soon)
	if calls.Load() != 2 {
		t.Fatalf("expired token served from cache (calls=%d)", calls.Load())
	}
	// Authorization is re-checked on cached tokens.
	now = now.Add(-2 * time.Minute)
	a.cache = nil
	_, _ = a.Authenticate(ctx, soon)
	allowed = false
	if _, err := a.Authenticate(ctx, soon); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked identity still accepted from cache: %v", err)
	}
}

func TestTokenReviewAuthenticator_WrongAudience(t *testing.T) {
	var calls atomic.Int32
	a := &TokenReviewAuthenticator{
		Client:  reviewer(t, map[string]string{"t": engineUser}, []string{"https://kubernetes.default.svc"}, &calls),
		Allowed: allowEngines,
	}
	if _, err := a.Authenticate(context.Background(), "t"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("token for another audience accepted: %v", err)
	}
}
