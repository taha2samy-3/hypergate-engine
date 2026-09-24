package firewall

import (
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
)

// startWAF serves a fake HTTP WAF on a Unix socket that rejects bodies containing "attack".
func startWAF(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "fw.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "attack") {
			http.Error(w, "blocked by waf", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, &calls
}

func newFirewall(t *testing.T, sock string) *FirewallFilter {
	t.Helper()
	cfg, err := config.ParseFirewallFilterConfig(map[string]interface{}{
		"protocol":     "http",
		"socket_path":  sock,
		"inspect_body": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFirewallFilter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func newCtx(endOfStream bool) *engine.RequestContext {
	return &engine.RequestContext{
		Method:             "POST",
		Path:               "/submit",
		RequestEndOfStream: endOfStream,
		Headers:            map[string]string{},
		UpstreamShadow:     map[string]string{},
		DownstreamShadow:   map[string]string{},
	}
}

func TestFirewall_InspectsBodyInBodyPhase(t *testing.T) {
	sock, calls := startWAF(t)
	f := newFirewall(t, sock)
	exec := engine.NewChainExecutor()
	chain := engine.Chain{f}

	ctx := newCtx(false)
	_ = exec.Execute(ctx, chain, engine.PhaseRequestHeaders)
	if !ctx.RequestBodyRequired || calls.Load() != 0 {
		t.Fatalf("headers phase should request the body and defer inspection (required=%v calls=%d)", ctx.RequestBodyRequired, calls.Load())
	}

	ctx.RequestBody = []byte("payload with attack")
	_ = exec.Execute(ctx, chain, engine.PhaseRequestBody)
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one WAF call, got %d", calls.Load())
	}
	if !ctx.Blocked || ctx.ResponseStatus != http.StatusForbidden {
		t.Fatalf("malicious body not blocked: blocked=%v status=%d", ctx.Blocked, ctx.ResponseStatus)
	}
}

func TestFirewall_BodylessRequestInspectedImmediately(t *testing.T) {
	sock, calls := startWAF(t)
	f := newFirewall(t, sock)

	ctx := newCtx(true) // e.g. a GET: no body will follow
	_ = engine.NewChainExecutor().Execute(ctx, engine.Chain{f}, engine.PhaseRequestHeaders)
	if calls.Load() != 1 {
		t.Fatalf("bodyless request must still be inspected, calls=%d", calls.Load())
	}
	if ctx.RequestBodyRequired {
		t.Fatal("bodyless request must not ask Envoy to buffer a body")
	}
}
