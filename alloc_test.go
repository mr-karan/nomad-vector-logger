package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/zerodha/logf"
)

// TestGenerateConfig verifies that generateConfig produces valid Vector config
// from a set of Nomad allocations.
func TestGenerateConfig(t *testing.T) {
	// Create temp directories for test
	tempDir := t.TempDir()
	vectorConfigDir := filepath.Join(tempDir, "vector")
	nomadDataDir := filepath.Join(tempDir, "nomad")

	app := &App{
		log: logf.New(logf.Opts{}),
		opts: Opts{
			nomadDataDir:    nomadDataDir,
			vectorConfigDir: vectorConfigDir,
		},
		configUpdated: make(chan bool, 10),
	}

	// Create mock allocations
	allocs := map[string]*api.Allocation{
		"alloc-123": {
			ID:        "alloc-123",
			Namespace: "default",
			NodeName:  "node-1",
			JobID:     "my-job",
			TaskGroup: "web",
			TaskResources: map[string]*api.Resources{
				"nginx": {},
			},
		},
		"alloc-456": {
			ID:        "alloc-456",
			Namespace: "production",
			NodeName:  "node-2",
			JobID:     "api-service",
			TaskGroup: "api",
			TaskResources: map[string]*api.Resources{
				"app":     {},
				"sidecar": {},
			},
		},
	}

	// Generate config
	err := app.generateConfig(allocs)
	if err != nil {
		t.Fatalf("generateConfig failed: %v", err)
	}

	// Read generated config
	configPath := filepath.Join(vectorConfigDir, "nomad.toml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read generated config: %v", err)
	}

	config := string(content)

	// Verify expected content is present
	expectedSnippets := []string{
		// First allocation
		"[sources.source_nomad_alloc_alloc-123_nginx]",
		"type = \"file\"",
		".nomad.namespace = \"default\"",
		".nomad.job_name = \"my-job\"",
		".nomad.task_name = \"nginx\"",
		".nomad.alloc_id = \"alloc-123\"",
		// Second allocation - both tasks
		"[sources.source_nomad_alloc_alloc-456_app]",
		"[sources.source_nomad_alloc_alloc-456_sidecar]",
		".nomad.namespace = \"production\"",
		".nomad.job_name = \"api-service\"",
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(config, snippet) {
			t.Errorf("expected config to contain %q, but it didn't\nConfig:\n%s", snippet, config)
		}
	}

	// Verify transforms exist for each source
	expectedTransforms := []string{
		"[transforms.transform_nomad_alloc_alloc-123_nginx]",
		"[transforms.transform_nomad_alloc_alloc-456_app]",
		"[transforms.transform_nomad_alloc_alloc-456_sidecar]",
	}

	for _, transform := range expectedTransforms {
		if !strings.Contains(config, transform) {
			t.Errorf("expected config to contain %q, but it didn't", transform)
		}
	}
}

// TestGenerateConfigEmpty verifies that generateConfig handles empty allocations
// by cleaning up the config directory.
func TestGenerateConfigEmpty(t *testing.T) {
	tempDir := t.TempDir()
	vectorConfigDir := filepath.Join(tempDir, "vector")

	// Pre-create directory with a file
	err := os.MkdirAll(vectorConfigDir, 0750)
	if err != nil {
		t.Fatalf("failed to create vector config dir: %v", err)
	}
	dummyFile := filepath.Join(vectorConfigDir, "old-config.toml")
	err = os.WriteFile(dummyFile, []byte("old config"), 0640)
	if err != nil {
		t.Fatalf("failed to create dummy file: %v", err)
	}

	app := &App{
		log: logf.New(logf.Opts{}),
		opts: Opts{
			vectorConfigDir: vectorConfigDir,
		},
		configUpdated: make(chan bool, 10),
	}

	// Generate config with empty allocations
	emptyAllocs := map[string]*api.Allocation{}
	err = app.generateConfig(emptyAllocs)
	if err != nil {
		t.Fatalf("generateConfig failed: %v", err)
	}

	// Verify old file was cleaned up
	entries, err := os.ReadDir(vectorConfigDir)
	if err != nil {
		t.Fatalf("failed to read vector config dir: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("expected config dir to be empty, but found %d files", len(entries))
	}
}

// TestGenerateConfigLogPath verifies that log paths are correctly constructed.
func TestGenerateConfigLogPath(t *testing.T) {
	tempDir := t.TempDir()
	vectorConfigDir := filepath.Join(tempDir, "vector")
	nomadDataDir := "/opt/nomad/data/alloc"

	app := &App{
		log: logf.New(logf.Opts{}),
		opts: Opts{
			nomadDataDir:    nomadDataDir,
			vectorConfigDir: vectorConfigDir,
		},
		configUpdated: make(chan bool, 10),
	}

	allocs := map[string]*api.Allocation{
		"abc-def-123": {
			ID:        "abc-def-123",
			Namespace: "default",
			NodeName:  "node-1",
			JobID:     "job",
			TaskGroup: "group",
			TaskResources: map[string]*api.Resources{
				"task1": {},
			},
		},
	}

	err := app.generateConfig(allocs)
	if err != nil {
		t.Fatalf("generateConfig failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(vectorConfigDir, "nomad.toml"))
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}

	// Verify the log path is correctly formed
	expectedPath := "/opt/nomad/data/alloc/abc-def-123/alloc/logs/task1*"
	if !strings.Contains(string(content), expectedPath) {
		t.Errorf("expected log path %q in config, got:\n%s", expectedPath, content)
	}
}