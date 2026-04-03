package registry

import (
	"os"
	"testing"

	"github.com/robinojw/tldr/pkg/config"
)

func TestRegistryCRUD(t *testing.T) {
	// Use a temp dir
	tmp := t.TempDir()
	t.Setenv("TLDR_CONFIG_DIR", tmp)

	reg, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	// Add
	err = reg.AddServer(&config.ServerEntry{
		Name:      "test-server",
		Transport: config.TransportStdio,
		Command:   "echo",
		Args:      []string{"hello"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Get
	s, ok := reg.GetServer("test-server")
	if !ok {
		t.Fatal("expected to find test-server")
	}
	if s.Command != "echo" {
		t.Errorf("expected command=echo, got %s", s.Command)
	}

	// List
	all := reg.ListServers()
	if len(all) != 1 {
		t.Errorf("expected 1 server, got %d", len(all))
	}

	// Wrapped
	err = reg.SetWrapped("test-server", true)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := reg.WrappedServers()
	if len(wrapped) != 1 {
		t.Errorf("expected 1 wrapped server, got %d", len(wrapped))
	}

	// Remove
	err = reg.RemoveServer("test-server")
	if err != nil {
		t.Fatal(err)
	}
	_, ok = reg.GetServer("test-server")
	if ok {
		t.Error("expected test-server to be removed")
	}

	// Persistence: reopen
	reg2, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	all = reg2.ListServers()
	if len(all) != 0 {
		t.Errorf("expected 0 servers after removal, got %d", len(all))
	}
}

func TestRegistryFileCreated(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TLDR_CONFIG_DIR", tmp)

	reg, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	err = reg.AddServer(&config.ServerEntry{
		Name:      "s1",
		Transport: config.TransportHTTP,
		URL:       "https://example.com/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Check file exists
	path := config.ServersPath()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected servers.json to exist at %s: %v", path, err)
	}
}
