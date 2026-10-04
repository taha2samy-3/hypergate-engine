package jwt_auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/taha2samy/hypergate/internal/engine"
)

func hsToken(t *testing.T, secret string, claims map[string]interface{}) string {
	t.Helper()
	tok := jwt.New()
	for k, v := range claims {
		if err := tok.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	_ = tok.Set(jwt.ExpirationKey, time.Now().Add(time.Hour))
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.HS256, []byte(secret)))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

func ctxWith(headers map[string]string, path string) *engine.RequestContext {
	ctx := &engine.RequestContext{
		Path:             path,
		Headers:          map[string]string{},
		UpstreamShadow:   map[string]string{},
		DownstreamShadow: map[string]string{},
	}
	for k, v := range headers {
		ctx.Headers[k] = v
	}
	return ctx
}

func TestClientCannotSpoofMappedHeaders(t *testing.T) {
	f, err := NewFilter(Config{LocalSecret: "s", ClaimMappings: map[string]string{"sub": "x-user-id", "email": "x-user-email"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	token := hsToken(t, "s", map[string]interface{}{"sub": "alice"}) // no email claim
	ctx := ctxWith(map[string]string{
		"authorization": "Bearer " + token,
		"x-user-email":  "admin@corp.example", // forged by the client
	}, "/")
	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatal("valid token rejected")
	}
	if ctx.GetHeader("x-user-id") != "alice" {
		t.Fatal("sub not mapped")
	}
	if got := ctx.GetHeader("x-user-email"); got != "" {
		t.Fatalf("client-supplied identity header reached upstream: %q", got)
	}
}

func TestClaimNamesAreCaseSensitiveAndValuesFormatted(t *testing.T) {
	f, err := NewFilter(Config{LocalSecret: "s", ClaimMappings: map[string]string{
		"tenantId": "X-Tenant", "roles": "x-roles", "exp": "x-exp",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	token := hsToken(t, "s", map[string]interface{}{"tenantId": "acme", "roles": []string{"admin", "dev"}})
	ctx := ctxWith(map[string]string{"authorization": "BEARER " + token}, "/")
	_ = f.Execute(ctx)
	if ctx.Blocked {
		t.Fatal("upper-case BEARER scheme must be accepted")
	}
	if ctx.GetHeader("x-tenant") != "acme" {
		t.Fatalf("camelCase claim not mapped: %v", ctx.UpstreamShadow)
	}
	if ctx.GetHeader("x-roles") != "admin,dev" {
		t.Fatalf("list claim formatted as %q", ctx.GetHeader("x-roles"))
	}
	if exp := ctx.GetHeader("x-exp"); len(exp) != 10 {
		t.Fatalf("exp should be unix seconds, got %q", exp)
	}
}

func TestClaimStringDropsControlCharacters(t *testing.T) {
	if got := claimString("ok\r\nX-Injected: 1"); got != "okX-Injected: 1" {
		t.Fatalf("got %q", got)
	}
}

func TestIntrospectionTokenIsFormEncoded(t *testing.T) {
	var gotToken string
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		gotToken = gotForm.Get("token")
		_, _ = w.Write([]byte(`{"active": true, "sub": "svc"}`))
	}))
	defer srv.Close()
	f, err := NewFilter(Config{IntrospectionEndpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	opaque := "a+b/c=&token_type_hint=refresh_token"
	_ = f.Execute(ctxWith(map[string]string{"authorization": "Bearer " + opaque}, "/"))
	if gotToken != opaque || gotForm.Get("token_type_hint") != "" {
		t.Fatalf("token not form-encoded: token=%q form=%v", gotToken, gotForm)
	}
}

func TestIntrospectionHonoursAudience(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"active": true, "aud": ["other-api"]}`))
	}))
	defer srv.Close()
	f, err := NewFilter(Config{IntrospectionEndpoint: srv.URL, Audience: "my-api"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ctx := ctxWith(map[string]string{"authorization": "Bearer opaque"}, "/")
	_ = f.Execute(ctx)
	if !ctx.Blocked {
		t.Fatal("token for another audience must be rejected")
	}
	if ctx.GetDownstreamHeader("www-authenticate") == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}
}

func TestStripTokenForQueryAndCookie(t *testing.T) {
	q, err := NewFilter(Config{LocalSecret: "s", Source: "query", QueryParam: "access_token", StripToken: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	token := hsToken(t, "s", nil)
	ctx := ctxWith(nil, "/data?access_token="+token+"&page=2")
	_ = q.Execute(ctx)
	if ctx.Blocked || ctx.Path != "/data?page=2" {
		t.Fatalf("query token not stripped: blocked=%v path=%q", ctx.Blocked, ctx.Path)
	}

	c, err := NewFilter(Config{LocalSecret: "s", Source: "cookie", CookieName: "jwt", StripToken: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx = ctxWith(map[string]string{"cookie": "theme=dark; jwt=" + token}, "/")
	_ = c.Execute(ctx)
	if ctx.Blocked || ctx.GetHeader("cookie") != "theme=dark" {
		t.Fatalf("cookie token not stripped: %q", ctx.GetHeader("cookie"))
	}
}
