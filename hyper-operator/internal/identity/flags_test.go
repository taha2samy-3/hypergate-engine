package identity

import (
	"flag"
	"testing"
)

func TestConfigValidateAndServerOptions(t *testing.T) {
	parse := func(args ...string) *Config {
		t.Helper()
		var c Config
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		c.BindFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return &c
	}

	if err := parse().Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	if err := parse("--identity-server").Validate(); err == nil {
		t.Fatal("--identity-server without --identity-cache must be refused")
	}
	if err := parse("--identity-cache", "--identity-server").Validate(); err == nil {
		t.Fatal("--identity-server without a certificate must be refused")
	}
	if err := parse("--identity-cache", "--identity-server", "--identity-server-insecure").Validate(); err != nil {
		t.Fatal(err)
	}

	c := parse("--identity-cache", "--identity-server", "--identity-server-cert-dir=/certs", "--identity-server-address=:7000")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	opts := c.ServerOptions(testAuth)
	if opts.CertFile != "/certs/tls.crt" || opts.KeyFile != "/certs/tls.key" || opts.Insecure {
		t.Fatalf("options = %+v", opts)
	}
	if port, err := c.ServerPort(); err != nil || port != 7000 {
		t.Fatalf("port = %d %v", port, err)
	}
	if c.ServiceName != "hyper-operator-identity" {
		t.Fatalf("service name = %q", c.ServiceName)
	}
	c.ServerAddress = "bad"
	if _, err := c.ServerPort(); err == nil {
		t.Fatal("invalid address accepted")
	}
}
