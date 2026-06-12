package tsextractor

import "testing"

func TestExtractHTTPClientFacts(t *testing.T) {
	src := "class FeedbackApi {\n" +
		"  async createFeedback(data: any) {\n" +
		"    return this.makeRequest<any>('/api/settings/feedback', { method: 'POST' });\n" +
		"  }\n" +
		"  async respond(id: number) {\n" +
		"    return this.makeRequest<any>(`/api/settings/feedback/${id}/respond`, {\n" +
		"      method: 'PUT',\n" +
		"    });\n" +
		"  }\n" +
		"  async stats() {\n" +
		"    return this.makeRequest<any>('/api/settings/feedback/statistics');\n" + // default GET
		"  }\n" +
		"  async external() {\n" +
		"    return fetch('https://third.party/v1/thing', { method: 'GET' });\n" + // skipped
		"  }\n" +
		"}\n"

	ff := extractHTTPClientFacts([]byte(src), "src/lib/api/feedback.ts")

	byName := map[string]string{} // name -> method
	for _, f := range ff {
		if f.Props["role"] != "client" || f.Props["framework"] != "fetch" {
			t.Errorf("%s wrong props: %+v", f.Name, f.Props)
		}
		if f.Props["api"] != "feedback" {
			t.Errorf("%s api hint = %v, want feedback", f.Name, f.Props["api"])
		}
		byName[f.Name] = f.Props["method"].(string)
	}

	if len(byName) != 3 {
		t.Fatalf("expected 3 backend client routes (external skipped), got %d: %+v", len(byName), byName)
	}
	if byName["/api/settings/feedback"] != "POST" {
		t.Errorf("createFeedback: want POST, got %+v", byName)
	}
	if byName["/api/settings/feedback/{}/respond"] != "PUT" {
		t.Errorf("respond: want PUT with {} param, got %+v", byName)
	}
	if byName["/api/settings/feedback/statistics"] != "GET" {
		t.Errorf("stats: want default GET, got %+v", byName)
	}
	if _, found := byName["https://third.party/v1/thing"]; found {
		t.Errorf("external URL should have been skipped: %+v", byName)
	}
}
