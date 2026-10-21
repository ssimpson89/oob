package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestUpdateConfigConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.json")
	if err := (&Config{Defaults: Host{User: "admin", Pass: "pw"}, Hosts: map[string]Host{"seed": {IP: "192.0.2.1"}}}).Save(p); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := updateConfigAt(p, func(c *Config) error {
				c.Hosts[fmt.Sprintf("h%02d", i)] = Host{IP: "192.0.2.2"}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := LoadConfig(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hosts) != 25 || got.Defaults.Pass != "pw" {
		t.Errorf("lost updates: %d hosts, defaults %+v", len(got.Hosts), got.Defaults)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
}

func TestUpdateConfigNoUpdateAndError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hosts.json")
	if err := updateConfigAt(p, func(*Config) error { return ErrNoUpdate }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("ErrNoUpdate wrote the file")
	}
	want := fmt.Errorf("boom")
	if err := updateConfigAt(p, func(*Config) error { return want }); err != want {
		t.Errorf("got %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("a failed update wrote the file")
	}
}

func TestLookupSetsNameButSaveDoesNot(t *testing.T) {
	c := &Config{Hosts: map[string]Host{"web1": {IP: "192.0.2.1"}}}
	name, h, err := c.Lookup("web")
	if err != nil || name != "web1" || h.Name != "web1" {
		t.Fatalf("%q %+v %v", name, h, err)
	}
	p := filepath.Join(t.TempDir(), "hosts.json")
	c.Hosts["web1"] = h
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "" && containsName(string(b)) {
		t.Errorf("Name leaked into the saved config: %s", b)
	}
}

func containsName(s string) bool {
	for i := 0; i+6 <= len(s); i++ {
		if s[i:i+6] == `"Name"` || s[i:i+6] == `"name"` {
			return true
		}
	}
	return false
}
