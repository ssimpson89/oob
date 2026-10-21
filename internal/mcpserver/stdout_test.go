package mcpserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ssimpson89/oob/internal/bmc"
	"github.com/ssimpson89/oob/internal/config"
)

// A real driver runs a fake ipmitool: nothing may reach the process's stdout,
// ipmitool must get no stdin, and its text must come back in the tool result.
func TestRealDriverOutputStaysOffStdout(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
echo "Chassis Power is on"
echo "stderr says hello" >&2
if [ -z "$(cat)" ]; then echo "stdin was empty"; fi
`
	if err := os.WriteFile(filepath.Join(bin, "ipmitool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")

	h := start(t, Options{}, "accept")
	h.srv.be.Driver = func(name string, host config.Host) bmc.Driver { return bmc.DriverFor(name, host) }
	h.srv.be.IPMI = bmc.RunIPMI

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = w
	var captured []byte
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); captured, _ = io.ReadAll(r) }()

	var outs []string
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"power_status", map[string]any{"host": "node01"}},
		{"sensors", map[string]any{"host": "node01"}},
		{"sel_list", map[string]any{"host": "node01"}},
		{"power", map[string]any{"host": "node01", "action": "cycle"}},
		{"mc_reset", map[string]any{"host": "node01", "kind": "cold"}},
	} {
		out, isErr := h.call(c.tool, c.args)
		if isErr {
			t.Errorf("%s: %q", c.tool, out)
		}
		outs = append(outs, out)
	}
	os.Stdout = oldOut
	w.Close()
	wg.Wait()

	if len(captured) != 0 {
		t.Errorf("something was written to stdout: %q", captured)
	}
	for i, out := range outs {
		if !strings.Contains(out, "Chassis Power is on") || !strings.Contains(out, "stdin was empty") || !strings.Contains(out, "stderr says hello") {
			t.Errorf("result %d lacks ipmitool's output: %q", i, out)
		}
		if strings.Contains(out, secretPass) {
			t.Errorf("result %d leaks the password", i)
		}
	}
}

// Run serves the protocol on stdout and makes sure nothing else can: a stray
// print from anywhere in the process goes to stderr.
func TestRunKeepsStdoutForTheProtocol(t *testing.T) {
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	restored := false
	restore := func() {
		if !restored {
			os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
			restored = true
		}
	}
	defer restore()

	var stderr []byte
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); stderr, _ = io.ReadAll(errR) }()

	be := Backend{
		Config: func() (*config.Config, error) { return testConfig(), nil },
		Driver: func(name string, _ config.Host) bmc.Driver { return &strayDriver{} },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{Backend: be}) }()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "power_status", Arguments: map[string]any{"host": "node01"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	got := res.Content[0].(*mcp.TextContent).Text
	if strings.TrimSpace(got) != "Chassis Power is on" {
		t.Errorf("result %q: the stray print leaked into the result", got)
	}
	cs.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Logf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't return after the client left")
	}
	if os.Stdout != outW {
		t.Error("Run didn't put os.Stdout back")
	}
	restore()
	errW.Close()
	wg.Wait()
	if !strings.Contains(string(stderr), "STRAY") {
		t.Errorf("the stray print should have gone to stderr, which got %q", stderr)
	}
}

// strayDriver prints to the process's stdout directly, as a buggy driver might.
type strayDriver struct{ bmc.Driver }

func (strayDriver) Power(string) error {
	fmt.Println("STRAY")
	fmt.Fprintln(bmc.Stdout(), "Chassis Power is on")
	return nil
}
