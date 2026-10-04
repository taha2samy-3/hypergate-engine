package jwt_auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

func rsaKey(t *testing.T, kid string) (jwk.Key, jwk.Key) {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := jwk.FromRaw(raw)
	_ = priv.Set(jwk.KeyIDKey, kid)
	_ = priv.Set(jwk.AlgorithmKey, jwa.RS256)
	pub, _ := priv.PublicKey()
	return priv, pub
}

func TestUnknownKidTriggersJWKSRefresh(t *testing.T) {
	_, pubOld := rsaKey(t, "old")
	privNew, pubNew := rsaKey(t, "new")

	var current atomic.Value
	setOf := func(k jwk.Key) jwk.Set { s := jwk.NewSet(); _ = s.AddKey(k); return s }
	current.Store(setOf(pubOld))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(current.Load())
	}))
	defer srv.Close()

	f, err := NewFilter(Config{JWKSEndpoint: srv.URL, JWKSRefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// The identity provider rotates to a new signing key.
	current.Store(setOf(pubNew))
	tok := jwt.New()
	_ = tok.Set(jwt.ExpirationKey, time.Now().Add(time.Hour))
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, privNew))
	if err != nil {
		t.Fatal(err)
	}

	f.lastFetch.Store(0) // pretend the last fetch is older than the refresh gap
	ctx := ctxWith(map[string]string{"authorization": "Bearer " + string(signed)}, "/")
	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatal("token signed with a rotated key must be accepted after an on-demand refresh")
	}
}
