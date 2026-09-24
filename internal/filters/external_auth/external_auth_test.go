package external_auth

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
)

func newCtx() *engine.RequestContext {
	return &engine.RequestContext{
		Path:     "/app/orders?id=7",
		Method:   "POST",
		ClientIP: "10.0.0.9",
		Headers: map[string]string{
			":path":      "/app/orders?id=7",
			":method":    "POST",
			":authority": "shop.example.com",
			":scheme":    "https",
			"cookie":     "_oauth2_proxy=abc",
			"connection": "keep-alive",
		},
		UpstreamShadow:   map[string]string{},
		DownstreamShadow: map[string]string{},
	}
}

func parse(t *testing.T, raw map[string]interface{}) *config.ExternalAuthConfig {
	t.Helper()
	cfg, err := config.ParseExternalAuthConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startHTTPSidecar mimics oauth2-proxy's /oauth2/auth: 202 with X-Auth-Request-User when
// the session cookie is present, otherwise 401.
func startHTTPSidecar(t *testing.T) (string, *http.Request) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "auth.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	seen := &http.Request{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = *r.Clone(context.Background())
		if r.Header.Get("Cookie") == "_oauth2_proxy=abc" {
			w.Header().Set("X-Auth-Request-User", "alice")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="shop"`)
		http.Error(w, "login required", http.StatusUnauthorized)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, seen
}

func TestHTTP_AllowsAndForwardsRequestContext(t *testing.T) {
	sock, seen := startHTTPSidecar(t)
	f, err := NewExternalAuthFilter(parse(t, map[string]interface{}{
		"protocol":    "http",
		"socket_path": sock,
		"path":        "/oauth2/auth",
		"on_success": map[string]interface{}{
			"upstream_headers_to_add":    []interface{}{"X-Auth-Request-User"},
			"upstream_headers_to_remove": []interface{}{"cookie"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	ctx := newCtx()
	if err := f.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Blocked {
		t.Fatalf("authenticated request blocked: %d %s", ctx.ResponseStatus, ctx.ResponseBody)
	}
	if ctx.GetHeader("x-auth-request-user") != "alice" {
		t.Fatal("identity header from the sidecar was not injected upstream")
	}
	if ctx.GetHeader("cookie") != "" {
		t.Fatal("cookie should be stripped before forwarding upstream")
	}

	if seen.URL.Path != "/oauth2/auth" {
		t.Fatalf("check path: got %q", seen.URL.Path)
	}
	for header, want := range map[string]string{
		"X-Forwarded-Method": "POST",
		"X-Forwarded-Uri":    "/app/orders?id=7",
		"X-Forwarded-Host":   "shop.example.com",
		"X-Forwarded-Proto":  "https",
		"X-Forwarded-For":    "10.0.0.9",
	} {
		if got := seen.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if seen.Header.Get("Connection") == "keep-alive" && seen.Header.Get(":path") != "" {
		t.Error("pseudo or hop-by-hop headers leaked to the sidecar")
	}
}

func TestHTTP_DenyRelaysStatusBodyAndHeaders(t *testing.T) {
	sock, _ := startHTTPSidecar(t)
	f, err := NewExternalAuthFilter(parse(t, map[string]interface{}{
		"protocol":    "http",
		"socket_path": sock,
		"on_failure": map[string]interface{}{
			"downstream_pass_through_headers": []interface{}{"WWW-Authenticate"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	ctx := newCtx()
	delete(ctx.Headers, "cookie")
	_ = f.Execute(ctx)
	if !ctx.Blocked || ctx.ResponseStatus != http.StatusUnauthorized {
		t.Fatalf("expected 401, got blocked=%v status=%d", ctx.Blocked, ctx.ResponseStatus)
	}
	if ctx.GetDownstreamHeader("www-authenticate") == "" {
		t.Fatal("WWW-Authenticate not relayed to the client")
	}
}

func TestHTTP_SidecarDownFailsClosed(t *testing.T) {
	f, err := NewExternalAuthFilter(parse(t, map[string]interface{}{
		"protocol":    "http",
		"socket_path": filepath.Join(t.TempDir(), "missing.sock"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := newCtx()
	_ = f.Execute(ctx)
	if !ctx.Blocked || ctx.ResponseStatus != http.StatusInternalServerError {
		t.Fatalf("unreachable sidecar must fail closed, got blocked=%v status=%d", ctx.Blocked, ctx.ResponseStatus)
	}
}

type authServer struct {
	authv3.UnimplementedAuthorizationServer
	last *authv3.CheckRequest
}

func (a *authServer) Check(_ context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	a.last = req
	if req.GetAttributes().GetRequest().GetHttp().GetHeaders()["authorization"] == "Bearer good" {
		return &authv3.CheckResponse{
			Status: &status.Status{Code: int32(codes.OK)},
			HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
				Headers: []*corev3.HeaderValueOption{{Header: &corev3.HeaderValue{Key: "x-user", Value: "bob"}}},
			}},
		}, nil
	}
	return &authv3.CheckResponse{
		Status: &status.Status{Code: int32(codes.PermissionDenied)},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status: &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden},
			Body:   "nope",
		}},
	}, nil
}

func startGRPCSidecar(t *testing.T) (string, *authServer) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "authz.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	impl := &authServer{}
	authv3.RegisterAuthorizationServer(srv, impl)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return sock, impl
}

func TestGRPC_AllowAndDeny(t *testing.T) {
	sock, impl := startGRPCSidecar(t)
	f, err := NewExternalAuthFilter(parse(t, map[string]interface{}{
		"protocol":    "grpc",
		"socket_path": sock,
		"on_success":  map[string]interface{}{"upstream_headers_to_add": []interface{}{"x-user"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	ok := newCtx()
	ok.Headers["authorization"] = "Bearer good"
	_ = f.Execute(ok)
	if ok.Blocked || ok.GetHeader("x-user") != "bob" {
		t.Fatalf("expected allow with x-user, blocked=%v", ok.Blocked)
	}
	http := impl.last.GetAttributes().GetRequest().GetHttp()
	if http.GetHost() != "shop.example.com" || http.GetScheme() != "https" || http.GetPath() != "/app/orders?id=7" {
		t.Fatalf("check request missing request attributes: %+v", http)
	}
	if _, leaked := http.GetHeaders()[":path"]; leaked {
		t.Fatal("pseudo headers belong in dedicated fields, not the header map")
	}
	if impl.last.GetAttributes().GetSource().GetAddress().GetSocketAddress().GetAddress() != "10.0.0.9" {
		t.Fatal("client address not sent to the authorization service")
	}

	denied := newCtx()
	_ = f.Execute(denied)
	if !denied.Blocked || denied.ResponseStatus != 403 || denied.ResponseBody != "nope" {
		t.Fatalf("expected 403 nope, got %v %d %q", denied.Blocked, denied.ResponseStatus, denied.ResponseBody)
	}
}
