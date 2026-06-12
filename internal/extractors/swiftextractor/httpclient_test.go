package swiftextractor

import "testing"

func TestExtractURLSessionFacts(t *testing.T) {
	src := `import Foundation

final class EntitlementAPIService {
    func getDefinitions() async throws -> [EntitlementDto] {
        var request = URLRequest(url: baseURL.appendingPathComponent("settings/entitlements/definitions"))
        request.httpMethod = "GET"
        return try await send(request)
    }

    func getActive(userID: Int) async throws -> [ActiveEntitlementDto] {
        var request = URLRequest(url: baseURL.appendingPathComponent("settings/entitlements/users/\(userID)/active"))
        return try await send(request)   // no explicit httpMethod -> default GET
    }

    func grant(userID: Int) async throws {
        var urlRequest = URLRequest(url: baseURL.appendingPathComponent("settings/entitlements/users/\(userID)/grant"))
        urlRequest.httpMethod = "POST"
        _ = try await send(urlRequest)
    }
}
`
	ff := extractURLSessionFacts([]byte(src), "Data/Network/EntitlementAPIService.swift")
	if len(ff) != 3 {
		t.Fatalf("expected 3 client routes, got %d: %+v", len(ff), ff)
	}

	byName := map[string]string{} // name -> method
	for _, f := range ff {
		if f.Props["role"] != "client" || f.Props["framework"] != "urlsession" {
			t.Errorf("%s wrong props: %+v", f.Name, f.Props)
		}
		if f.Props["api"] != "EntitlementAPIService" {
			t.Errorf("%s api hint = %v", f.Name, f.Props["api"])
		}
		byName[f.Name] = f.Props["method"].(string)
	}

	if byName["settings/entitlements/definitions"] != "GET" {
		t.Errorf("definitions: want GET, got %q", byName["settings/entitlements/definitions"])
	}
	// Interpolation collapsed to {}, no httpMethod line -> default GET.
	if byName["settings/entitlements/users/{}/active"] != "GET" {
		t.Errorf("active: want default GET on {}, got %+v", byName)
	}
	if byName["settings/entitlements/users/{}/grant"] != "POST" {
		t.Errorf("grant: want POST, got %+v", byName)
	}
}
