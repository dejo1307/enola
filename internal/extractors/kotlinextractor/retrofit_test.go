package kotlinextractor

import "testing"

func TestExtractRetrofitFacts(t *testing.T) {
	src := `package com.fairwayhub.mygolfjournal.data.api

interface EntitlementApiService {
    @GET("/api/settings/entitlements/users/{userID}/active")
    suspend fun getActiveEntitlements(@Path("userID") userID: Int): Response<List<ActiveEntitlementDto>>

    @POST("auth/login")
    @Headers("X-Client-Type: mobile")
    suspend fun login(@Body request: LoginRequest): Response<LoginResponse>
}
`
	ff := extractRetrofitFacts([]byte(src), "data/api/EntitlementApiService.kt")
	if len(ff) != 2 {
		t.Fatalf("expected 2 client routes, got %d: %+v", len(ff), ff)
	}

	byName := map[string]map[string]any{}
	for _, f := range ff {
		if f.Kind != "route" {
			t.Errorf("kind = %q, want route", f.Kind)
		}
		if f.Props["role"] != "client" {
			t.Errorf("%s role = %v, want client", f.Name, f.Props["role"])
		}
		if f.Props["framework"] != "retrofit" {
			t.Errorf("%s framework = %v, want retrofit", f.Name, f.Props["framework"])
		}
		if f.Props["api"] != "EntitlementApiService" {
			t.Errorf("%s api hint = %v, want EntitlementApiService", f.Name, f.Props["api"])
		}
		byName[f.Name] = f.Props
	}

	if p, ok := byName["/api/settings/entitlements/users/{userID}/active"]; !ok || p["method"] != "GET" {
		t.Errorf("missing GET entitlements route; got %+v", byName)
	}
	if p, ok := byName["auth/login"]; !ok || p["method"] != "POST" {
		t.Errorf("missing POST auth/login route (relative path); got %+v", byName)
	}
}
