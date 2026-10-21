package bmc

import "testing"

func TestSerialAndSizeLine(t *testing.T) {
	sol := &solLink{}
	if !sol.Serial() {
		t.Error("SOL is a serial line")
	}
	if line, ok := sol.SizeLine(50, 200); !ok || line != "stty rows 50 cols 200" {
		t.Errorf("SOL size line = %q, %v", line, ok)
	}
	console := &cliLink{sshLink: &sshLink{}}
	if !console.Serial() {
		t.Error("a vendor console is a serial line")
	}
	if line, ok := console.SizeLine(50, 200); !ok || line != "stty rows 50 cols 200" {
		t.Errorf("console size line = %q, %v", line, ok)
	}
	console.atPrompt = true
	if console.Serial() {
		t.Error("the BMC's own CLI prompt is ssh, not a serial line")
	}
	plain := &cliLink{sshLink: &sshLink{}, plain: true}
	if _, ok := plain.SizeLine(50, 200); ok {
		t.Error("plain ssh sends a window change and types nothing")
	}
}
