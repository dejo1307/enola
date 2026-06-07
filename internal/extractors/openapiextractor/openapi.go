package openapiextractor

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"gopkg.in/yaml.v3"
)

// OpenAPIExtractor extracts route facts from OpenAPI 3.x spec files (YAML/JSON).
// It performs its own directory scan rather than relying on the engine walker,
// because OpenAPI specs are YAML/JSON files that are typically excluded from the
// main walker by the global *.yml/yaml/json ignore rules.
type OpenAPIExtractor struct{}

// New creates a new OpenAPIExtractor.
func New() *OpenAPIExtractor {
	return &OpenAPIExtractor{}
}

func (e *OpenAPIExtractor) Name() string {
	return "openapi"
}

// Detect returns true if the repository contains any OpenAPI spec files.
func (e *OpenAPIExtractor) Detect(repoPath string) (bool, error) {
	found := false
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if isOpenAPICandidate(path) && hasOpenAPIContent(path) {
			found = true
		}
		return nil
	})
	return found, err
}

// Extract scans the repository for OpenAPI spec files and emits KindRoute facts
// enriched with operationId, summary, tags, and a spec_file back-reference.
// The files argument (from the engine walker) is intentionally ignored because
// YAML files are typically excluded by the global ignore patterns.
func (e *OpenAPIExtractor) Extract(ctx context.Context, repoPath string, _ []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		if !isOpenAPICandidate(path) {
			return nil
		}

		relFile, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			return nil
		}

		routeFacts, parseErr := parseOpenAPIFile(path, relFile)
		if parseErr != nil {
			log.Printf("[openapi-extractor] skipping %s: %v", relFile, parseErr)
			return nil
		}
		allFacts = append(allFacts, routeFacts...)
		return nil
	})

	return allFacts, err
}

// openAPISpec is the minimal structure needed to extract route facts.
type openAPISpec struct {
	OpenAPI string                            `yaml:"openapi"`
	Swagger string                            `yaml:"swagger"`
	Info    openAPIInfo                       `yaml:"info"`
	Paths   map[string]map[string]interface{} `yaml:"paths"`
}

// openAPIInfo captures the info block, including custom gateway config extensions.
type openAPIInfo struct {
	// x-gateway-config is a custom extension that configures the API Gateway.
	// Example: { at-gateway-prefix: "/svc-example" }
	GatewayConfig map[string]interface{} `yaml:"x-gateway-config"`
}

// httpMethods maps lowercase OpenAPI method keys to their canonical HTTP form.
var httpMethods = map[string]string{
	"get":     "GET",
	"post":    "POST",
	"put":     "PUT",
	"patch":   "PATCH",
	"delete":  "DELETE",
	"head":    "HEAD",
	"options": "OPTIONS",
	"trace":   "TRACE",
}

// parseOpenAPIFile reads and parses a single OpenAPI spec file, returning
// one KindRoute fact per operation defined in the spec.
func parseOpenAPIFile(absPath, relFile string) ([]facts.Fact, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}

	var spec openAPISpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}

	if spec.OpenAPI == "" && spec.Swagger == "" {
		return nil, fmt.Errorf("not an OpenAPI spec (missing openapi/swagger field)")
	}

	if len(spec.Paths) == 0 {
		return nil, nil
	}

	// Client specs (e.g. api/openapi/client/svc-foo.yml) represent routes that
	// THIS service calls on ANOTHER service. They are distinct from routes this
	// service serves and must be marked accordingly to avoid polluting routing queries.
	role := "server"
	if isClientSpec(relFile) {
		role = "client"
	}

	// Extract the API Gateway prefix from info.x-gateway-config.
	// When set, the gateway routes requests at "<prefix><path>" to this service.
	gatewayPrefix := ""
	if prefix, ok := spec.Info.GatewayConfig["at-gateway-prefix"].(string); ok {
		gatewayPrefix = strings.TrimRight(prefix, "/")
	}

	specDir := filepath.ToSlash(filepath.Dir(relFile))
	var result []facts.Fact

	for path, pathItem := range spec.Paths {
		if pathItem == nil {
			continue
		}
		for methodKey, opRaw := range pathItem {
			httpMethod, ok := httpMethods[strings.ToLower(methodKey)]
			if !ok {
				// Non-method keys like "parameters", "summary", "servers" — skip.
				continue
			}

			props := map[string]any{
				"method":    httpMethod,
				"source":    "openapi",
				"spec_file": relFile,
				"framework": "openapi",
				"language":  "openapi",
				"role":      role,
			}

			if gatewayPrefix != "" {
				props["gateway_prefix"] = gatewayPrefix
				props["gateway_path"] = gatewayPrefix + path
			}

			if opRaw != nil {
				if opMap, ok := opRaw.(map[string]interface{}); ok {
					if v, ok := opMap["operationId"].(string); ok && v != "" {
						props["operationId"] = v
					}
					if v, ok := opMap["summary"].(string); ok && v != "" {
						props["summary"] = v
					}
					if v, ok := opMap["description"].(string); ok && v != "" {
						props["description"] = v
					}
					if tags, ok := opMap["tags"].([]interface{}); ok && len(tags) > 0 {
						tagStrings := make([]string, 0, len(tags))
						for _, t := range tags {
							if ts, ok := t.(string); ok {
								tagStrings = append(tagStrings, ts)
							}
						}
						if len(tagStrings) > 0 {
							props["tags"] = tagStrings
						}
					}
				// x-gateway-capabilities is a custom extension marking which
				// operations are exposed at the API Gateway and their auth requirements.
					if caps, ok := opMap["x-gateway-capabilities"].(map[string]interface{}); ok {
						if exposed, ok := caps["exposed"].(bool); ok {
							props["exposed"] = exposed
						}
						if auth, ok := caps["auth"].(map[string]interface{}); ok {
							if mode, ok := auth["mode"].(string); ok && mode != "" {
								props["auth_mode"] = mode
							}
						}
					}
				}
			}

			result = append(result, facts.Fact{
				Kind:  facts.KindRoute,
				Name:  path,
				File:  relFile,
				Props: props,
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: specDir},
				},
			})
		}
	}

	return result, nil
}

// isClientSpec returns true when the spec file lives inside a "client" directory
// that is itself inside an "openapi" directory. These specs describe the API of
// another service that this repo calls, not routes this service serves.
// Examples: api/openapi/client/svc-foo.yml, packages/x/api/openapi/client/svc-bar.yml
func isClientSpec(relFile string) bool {
	parts := strings.Split(filepath.ToSlash(relFile), "/")
	for i, part := range parts {
		if part == "client" && i > 0 && parts[i-1] == "openapi" {
			return true
		}
	}
	return false
}

// isOpenAPICandidate returns true if the file path looks like an OpenAPI spec
// based on naming conventions, without reading the file.
func isOpenAPICandidate(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yml" && ext != ".yaml" && ext != ".json" {
		return false
	}

	base := strings.ToLower(filepath.Base(path))

	// Skip oapi-codegen configuration files (e.g. fee.gen.yml).
	if strings.HasSuffix(base, ".gen.yml") || strings.HasSuffix(base, ".gen.yaml") {
		return false
	}
	// Skip partial/fragment specs (meant to be merged before use).
	if strings.Contains(base, ".partial.") {
		return false
	}

	// Files whose name explicitly references openapi or swagger.
	if strings.Contains(base, "openapi") || strings.Contains(base, "swagger") {
		return true
	}

	// Files located inside a directory segment named "openapi".
	slashPath := filepath.ToSlash(path)
	for _, part := range strings.Split(slashPath, "/") {
		if part == "openapi" {
			return true
		}
	}

	return false
}

// hasOpenAPIContent reads the first 512 bytes of a file and checks for the
// presence of "openapi:" or "swagger:" keys, confirming it's a spec file.
func hasOpenAPIContent(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	content := string(buf[:n])
	return strings.Contains(content, "openapi:") || strings.Contains(content, "swagger:")
}

// skipDir returns true for directories that should never be descended into.
func skipDir(name string) bool {
	switch name {
	case "vendor", "node_modules", ".git", ".enola", "backstage",
		"tmp", "log", "build", ".build", ".gradle":
		return true
	}
	return false
}
