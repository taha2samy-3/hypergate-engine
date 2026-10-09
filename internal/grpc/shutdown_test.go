package grpc

import (
	"context"
	"net"
	"testing"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/taha2samy/hypergate/internal/config"
	"github.com/taha2samy/hypergate/internal/engine"
)

func TestWaitForQuiet(t *testing.T) {
	ctx := context.Background()

	t.Run("no traffic waits the minimum", func(t *testing.T) {
		got := WaitForQuiet(ctx, &Activity{}, 50*time.Millisecond, 300*time.Millisecond, 2*time.Second)
		if got < 300*time.Millisecond || got > 700*time.Millisecond {
			t.Fatalf("waited %s, want about the 300ms minimum", got)
		}
	})

	t.Run("keeps waiting while streams arrive, then stops after quiet", func(t *testing.T) {
		a := &Activity{}
		stop := make(chan struct{})
		go func() {
			deadline := time.Now().Add(600 * time.Millisecond)
			for time.Now().Before(deadline) {
				a.streamStarted()
				a.streamEnded()
				time.Sleep(20 * time.Millisecond)
			}
			close(stop)
		}()
		got := WaitForQuiet(ctx, a, 300*time.Millisecond, 0, 5*time.Second)
		<-stop
		if got < 850*time.Millisecond || got > 1500*time.Millisecond {
			t.Fatalf("waited %s, want about 600ms of traffic + 300ms quiet", got)
		}
	})

	t.Run("never waits past the maximum", func(t *testing.T) {
		a := &Activity{}
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					a.streamStarted()
					a.streamEnded()
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()
		got := WaitForQuiet(ctx, a, time.Second, 0, 400*time.Millisecond)
		if got < 400*time.Millisecond || got > 800*time.Millisecond {
			t.Fatalf("waited %s, want the 400ms maximum", got)
		}
	})

	t.Run("cancellation stops the wait", func(t *testing.T) {
		c, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if got := WaitForQuiet(c, &Activity{}, time.Second, 5*time.Second, 10*time.Second); got > time.Second {
			t.Fatalf("waited %s after cancellation", got)
		}
	})
}

// hangingProcessor keeps every stream open until the server closes it.
type hangingProcessor struct {
	extprocv3.UnimplementedExternalProcessorServer
	started chan struct{}
}

func (h *hangingProcessor) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	h.started <- struct{}{}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestStopWithin(t *testing.T) {
	start := func(t *testing.T) (*grpc.Server, *hangingProcessor, extprocv3.ExternalProcessorClient) {
		t.Helper()
		lis := bufconn.Listen(1 << 20)
		srv := grpc.NewServer()
		h := &hangingProcessor{started: make(chan struct{}, 1)}
		extprocv3.RegisterExternalProcessorServer(srv, h)
		go func() { _ = srv.Serve(lis) }()
		conn, err := grpc.NewClient("passthrough:///engine",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return srv, h, extprocv3.NewExternalProcessorClient(conn)
	}

	t.Run("idle server stops gracefully", func(t *testing.T) {
		srv, _, _ := start(t)
		if forced := StopWithin(srv, time.Second); forced {
			t.Fatal("an idle server should stop without forcing")
		}
	})

	t.Run("a stream that never ends is closed after the timeout", func(t *testing.T) {
		srv, h, client := start(t)
		stream, err := client.Process(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&extprocv3.ProcessingRequest{}); err != nil {
			t.Fatal(err)
		}
		<-h.started
		begin := time.Now()
		if forced := StopWithin(srv, 200*time.Millisecond); !forced {
			t.Fatal("expected the open stream to be closed by force")
		}
		if took := time.Since(begin); took > time.Second {
			t.Fatalf("stop took %s", took)
		}
		if _, err := stream.Recv(); err == nil {
			t.Fatal("the stream should be closed")
		}
	})
}

func TestProcess_RecordsActivity(t *testing.T) {
	a := &Activity{}
	s := newTestServer(&config.Config{}, map[string]engine.Chain{})
	s.activity = a
	before := time.Now()
	run(t, s, reqHeaders("/", true))
	if a.LastStart().Before(before) || a.Active() != 0 {
		t.Fatalf("activity not recorded: last=%s active=%d", a.LastStart(), a.Active())
	}
}
