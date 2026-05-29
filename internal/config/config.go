package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the mcp-arch.yaml configuration.
type Config struct {
	Repo       string       `yaml:"repo"`
	Ignore     []string     `yaml:"ignore"`
	Extractors []string     `yaml:"extractors"`
	Explainers []string     `yaml:"explainers"`
	Renderers  []string     `yaml:"renderers"`
	Output     OutputConfig `yaml:"output"`
}

// OutputConfig controls where and how output artifacts are generated.
type OutputConfig struct {
	Dir              string `yaml:"dir"`
	MaxContextTokens int    `yaml:"max_context_tokens"`
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Repo: ".",
		Ignore: []string{
			"vendor/**",
			"node_modules/**",
			".git/**",
			"**/*_test.go",
			"**/*.test.ts",
			"**/*.test.tsx",
			"**/*.spec.ts",
			"**/*.spec.tsx",
			"**/*_spec.rb",
			"**/*_test.rb",
			".enola/**",
		},
		Extractors: []string{"go", "kotlin", "openapi", "python", "typescript", "swift", "ruby"},
		Explainers: []string{"cycles", "layers"},
		Renderers:  []string{"llm_context"},
		Output: OutputConfig{
			Dir:              ".enola",
			MaxContextTokens: 16000,
		},
	}
}

// Load reads a configuration file from the given path.
// Missing fields are filled with defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// Ensure required defaults
	if cfg.Output.Dir == "" {
		cfg.Output.Dir = ".enola"
	}
	if cfg.Output.MaxContextTokens == 0 {
		cfg.Output.MaxContextTokens = 16000
	}

	return cfg, nil
}

// IsExtractorEnabled returns true if the named extractor is enabled.
func (c *Config) IsExtractorEnabled(name string) bool {
	return contains(c.Extractors, name)
}

// IsExplainerEnabled returns true if the named explainer is enabled.
func (c *Config) IsExplainerEnabled(name string) bool {
	return contains(c.Explainers, name)
}

// IsRendererEnabled returns true if the named renderer is enabled.
func (c *Config) IsRendererEnabled(name string) bool {
	return contains(c.Renderers, name)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
