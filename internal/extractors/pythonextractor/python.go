package pythonextractor

import (
	"context"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// PythonExtractor extracts architectural facts from Python source code using
// line-based regex parsing with indentation-based scope tracking.
type PythonExtractor struct{}

// New creates a new PythonExtractor.
func New() *PythonExtractor {
	return &PythonExtractor{}
}

func (e *PythonExtractor) Name() string {
	return "python"
}

// Detect returns true if the repository looks like a Python project.
// It checks root-level markers first, then walks up to 3 subdirectory levels
// to support monorepos where Python code lives in a subdirectory (e.g. python/).
func (e *PythonExtractor) Detect(repoPath string) (bool, error) {
	// Root-level markers — fast path.
	rootMarkers := []string{
		"pyproject.toml", "setup.py", "requirements.txt", "Pipfile",
		"pytest.ini", "mypy.ini", "tox.ini", "setup.cfg",
	}
	for _, name := range rootMarkers {
		if _, err := os.Stat(filepath.Join(repoPath, name)); err == nil {
			return true, nil
		}
	}

	// Subdirectory search (up to 3 levels deep) — handles monorepos.
	subMarkers := map[string]bool{
		"pyproject.toml":   true,
		"setup.py":         true,
		"requirements.txt": true,
		"Pipfile":          true,
	}
	found := false
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		rel, _ := filepath.Rel(repoPath, path)
		depth := strings.Count(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if depth >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if subMarkers[filepath.Base(path)] {
			found = true
		}
		return nil
	})
	return found, nil
}

// Extract parses Python files and emits architectural facts.
func (e *PythonExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact
	modules := make(map[string]bool)
	isDjango := detectDjango(repoPath)

	for _, relFile := range files {
		select {
		case <-ctx.Done():
			return allFacts, ctx.Err()
		default:
		}

		if !isPythonFile(relFile) {
			continue
		}

		absFile := filepath.Join(repoPath, relFile)
		f, err := os.Open(absFile)
		if err != nil {
			log.Printf("[python-extractor] error reading %s: %v", relFile, err)
			continue
		}

		src, readErr := readAll(f)
		f.Close()
		var fileFacts []facts.Fact
		if readErr != nil {
			log.Printf("[python-extractor] error reading %s: %v", relFile, readErr)
			continue
		}
		fileFacts = extractFileAST(src, relFile, isDjango)
		allFacts = append(allFacts, fileFacts...)

		dir := filepath.Dir(relFile)
		modules[dir] = true
	}

	for dir := range modules {
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language": "python",
			},
		})
	}

	return allFacts, nil
}

// --- Regex patterns used by the AST walker ---

var (
	// routeDecoratorRe matches FastAPI/Starlette route decorators.
	// Groups: (object, http_method, path).
	routeDecoratorRe = regexp.MustCompile(`^\s*@([\w.]+)\.(get|post|put|delete|patch|head|options)\s*\(\s*["']([^"']+)["']`)

	// tableNameRe matches SQLAlchemy __tablename__ assignments. Group: (table).
	tableNameRe = regexp.MustCompile(`^\s*__tablename__\s*=\s*["']([^"']+)["']`)

	// decoratorRe captures the full decorator name for structural prop detection.
	// Group: (name) e.g. "staticmethod", "app.task".
	decoratorRe = regexp.MustCompile(`^\s*@([\w.]+)`)

	// apiViewRe matches Django REST Framework @api_view decorators.
	// Group: (methods_list) — bracket contents, e.g. "'GET', 'POST'"
	apiViewRe = regexp.MustCompile(`^\s*@(?:[\w.]*\.)?api_view\s*\(\s*\[([^\]]+)\]`)

	// httpMethodWordRe extracts uppercase HTTP method tokens from an api_view list.
	httpMethodWordRe = regexp.MustCompile(`[A-Z]+`)

	// urlPathRe matches Django path() and re_path() calls in urls.py.
	// Groups: (url_path, view_ref)
	urlPathRe = regexp.MustCompile(`(?:re_)?path\s*\(\s*r?["']([^"']+)["']\s*,\s*([\w.]+)`)
)

// Django class base sets used to classify models, views, and serializers.
var (
	djangoModelBases = map[string]bool{
		"Model": true, "AbstractModel": true, "MPTTModel": true,
		"TimeStampedModel": true, "UUIDModel": true, "PolymorphicModel": true,
	}

	djangoCBVBases = map[string]bool{
		"View": true, "APIView": true, "GenericAPIView": true,
		"ListAPIView": true, "CreateAPIView": true, "RetrieveAPIView": true,
		"UpdateAPIView": true, "DestroyAPIView": true, "ListCreateAPIView": true,
		"RetrieveUpdateDestroyAPIView": true, "ViewSet": true, "ModelViewSet": true,
		"ReadOnlyModelViewSet": true, "TemplateView": true, "DetailView": true,
		"ListView": true, "CreateView": true, "UpdateView": true, "DeleteView": true,
		"FormView": true, "RedirectView": true,
	}

	djangoSerializerBases = map[string]bool{
		"Serializer": true, "ModelSerializer": true,
		"HyperlinkedModelSerializer": true, "ListSerializer": true,
	}
)


// applyDecoratorProps sets structural boolean props on a symbol based on a
// decorator name. Only well-known structural decorators produce props; unknown
// decorators are silently ignored.
func applyDecoratorProps(props map[string]any, decoratorName string) {
	// Use the last dot-separated component: "functools.cached_property" → "cached_property".
	last := decoratorName
	if idx := strings.LastIndex(decoratorName, "."); idx >= 0 {
		last = decoratorName[idx+1:]
	}
	switch last {
	case "property", "cached_property":
		props["property"] = true
	case "staticmethod":
		props["static"] = true
	case "classmethod":
		props["class_method"] = true
	case "abstractmethod":
		props["abstract"] = true
	case "task":
		props["task"] = true
	case "shared_task":
		// shared_task is Celery-specific; bare @task is used by Airflow, Prefect, Luigi, etc.
		props["task"] = true
		props["framework"] = "celery"
	}
}

// detectDjango returns true if the project at repoPath uses Django, by scanning
// common dependency files and checking for manage.py.
func detectDjango(repoPath string) bool {
	for _, name := range []string{"requirements.txt", "pyproject.toml", "setup.cfg", "setup.py"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(string(data)), "django") {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(repoPath, "manage.py"))
	return err == nil
}

// camelToSnake converts a PascalCase class name to the snake_case table name
// Django would auto-generate. e.g. "UserProfile" → "user_profile".
func camelToSnake(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if i > 0 && ch >= 'A' && ch <= 'Z' {
			b.WriteByte('_')
		}
		if ch >= 'A' && ch <= 'Z' {
			b.WriteByte(ch + 32) // ASCII lowercase
		} else {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// lastComponent returns the last dot-separated segment of a qualified name.
// e.g. "models.Model" → "Model", "Model" → "Model".
func lastComponent(name string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		return name[idx+1:]
	}
	return name
}


// isPythonFile returns true if the file has a .py extension.
func isPythonFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".py")
}

// readAll reads all bytes from an open file, seeking to the start first.
func readAll(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}
