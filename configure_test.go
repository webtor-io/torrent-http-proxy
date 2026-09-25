package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// The probe answers Ready as soon as it listens, and the DaemonSet's
// maxSurge 1 retires the old pod on that. run() binds the web port before
// any servable starts, so a pod whose web port cannot be bound exits without
// ever serving the probe.
func TestRunBindsWebBeforeProbe(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("default:\n  name: torrent-web-seeder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := &logtest.Hook{}
	old := log.StandardLogger().ReplaceHooks(log.LevelHooks{})
	log.AddHook(hook)
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(old) })

	app := cli.NewApp()
	configure(app)
	err = app.Run([]string{"torrent-http-proxy",
		"--config", cfg,
		"--host", "127.0.0.1", "--port", strconv.Itoa(busy.Addr().(*net.TCPAddr).Port),
		"--probe-host", "127.0.0.1", "--probe-port", strconv.Itoa(freePort(t)),
		"--use-prom=false", "--use-pprof=false",
	})
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("run with the web port taken returned %v, want the listen error", err)
	}
	// A probe started alongside would log from its own goroutine.
	time.Sleep(200 * time.Millisecond)
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "serving probe") {
			t.Errorf("the probe was served although the web port was never bound")
		}
	}
}

// The chart sets WEB_SHUTDOWN_TIMEOUT; without the flag registered the drain
// timeout would silently be 0 and every in-flight request cut at once.
func TestConfigureRegistersShutdownTimeout(t *testing.T) {
	t.Setenv("WEB_SHUTDOWN_TIMEOUT", "60s")
	app := cli.NewApp()
	configure(app)
	var got time.Duration
	app.Action = func(c *cli.Context) error {
		got = cs.ShutdownTimeout(c)
		return nil
	}
	if err := app.Run([]string{"torrent-http-proxy", "--config", "unused.yaml"}); err != nil {
		t.Fatal(err)
	}
	if got != 60*time.Second {
		t.Errorf("shutdown timeout %v from WEB_SHUTDOWN_TIMEOUT=60s, want 60s", got)
	}
}
