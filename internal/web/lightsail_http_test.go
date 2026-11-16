package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"reladock/internal/cloud/lightsail"
	"reladock/internal/platform"
)

type fakeLightsailCatalogProvider struct {
	lightsail.Provider
	calls   int
	catalog lightsail.Catalog
	err     error
}

func (f *fakeLightsailCatalogProvider) Catalog(_ context.Context, region string) (lightsail.Catalog, error) {
	f.calls++
	if f.err != nil {
		return lightsail.Catalog{}, f.err
	}
	result := f.catalog
	result.Region = region
	return result, nil
}

func lightsailHTTPConfig(enabled bool) lightsail.Config {
	return lightsail.Config{Enabled: enabled, AllowedRegions: []string{"us-east-1"}, SSHSourceCIDR: "192.0.2.8/32", WorkerInterval: time.Second, AWSOperationTimeout: time.Minute, SSHConnectTimeout: time.Second, ScriptTimeout: time.Minute}
}

func lightsailCatalogFixture() lightsail.Catalog {
	return lightsail.Catalog{
		AvailabilityZones: []lightsail.AvailabilityZone{{Name: "us-east-1a", State: "available"}},
		Blueprints:        []lightsail.Blueprint{{ID: "debian", Name: "Debian", Platform: "LINUX_UNIX", Active: true}, {ID: "windows", Name: "Windows", Platform: "WINDOWS", Active: true}, {ID: "old", Name: "Old", Platform: "LINUX_UNIX", Active: false}},
		Bundles:           []lightsail.Bundle{{ID: "small", Name: "Small", Active: true, SupportedPlatforms: []string{"LINUX_UNIX"}}, {ID: "windows-small", Name: "Windows", Active: true, SupportedPlatforms: []string{"WINDOWS"}}},
	}
}

func newLightsailAdminSession(t *testing.T, store *platform.Store, handler http.Handler, email string) *http.Cookie {
	t.Helper()
	registered := request(handler, http.MethodPost, "/api/auth/register", `{"company":"Lightsail","email":"`+email+`","password":"long-test-password-42"}`, nil)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	if err := store.SetPlatformAdmin(context.Background(), email, true); err != nil {
		t.Fatal(err)
	}
	loggedIn := request(handler, http.MethodPost, "/api/auth/login", `{"email":"`+email+`","password":"long-test-password-42"}`, nil)
	if loggedIn.Code != http.StatusOK || len(loggedIn.Result().Cookies()) == 0 {
		t.Fatalf("login: %d %s", loggedIn.Code, loggedIn.Body.String())
	}
	return loggedIn.Result().Cookies()[0]
}

func lightsailRequest(method, path, body string, cookie *http.Cookie, idempotencyKey string) *http.Request {
	req := httptest.NewRequest(method, "http://127.0.0.1:3001"+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "RelaDock")
	req.Header.Set("Origin", "http://127.0.0.1:3001")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func TestLightsailHTTPDisabledStatusDoesNotCallProvider(t *testing.T) {
	store := testWebStore(t)
	provider := &fakeLightsailCatalogProvider{catalog: lightsailCatalogFixture()}
	handler := NewWithOptions(store, fstest.MapFS{"index.html": {Data: []byte("ok")}}, "http://127.0.0.1:3001", Options{Lightsail: &LightsailOptions{Config: lightsailHTTPConfig(false), Provider: provider}})
	cookie := newLightsailAdminSession(t, store, handler, "lightsail-disabled@example.com")
	response := serve(handler, lightsailRequest(http.MethodGet, "/api/admin/lightsail/status", "", cookie, ""))
	if response.Code != http.StatusOK || response.Body.String() != "{\"enabled\":false}\n" || provider.calls != 0 {
		t.Fatalf("status=%d body=%s calls=%d", response.Code, response.Body.String(), provider.calls)
	}
}

func TestLightsailHTTPIsPlatformAdminOnlyAndRejectsBearer(t *testing.T) {
	store := testWebStore(t)
	provider := &fakeLightsailCatalogProvider{catalog: lightsailCatalogFixture()}
	handler := NewWithOptions(store, fstest.MapFS{"index.html": {Data: []byte("ok")}}, "http://127.0.0.1:3001", Options{Lightsail: &LightsailOptions{Config: lightsailHTTPConfig(true), Provider: provider}})
	registered := request(handler, http.MethodPost, "/api/auth/register", `{"company":"Tenant","email":"lightsail-tenant@example.com","password":"long-test-password-42"}`, nil)
	tenantCookie := registered.Result().Cookies()[0]
	if response := serve(handler, lightsailRequest(http.MethodGet, "/api/admin/lightsail/instances", "", tenantCookie, "")); response.Code != http.StatusForbidden {
		t.Fatalf("tenant status=%d body=%s", response.Code, response.Body.String())
	}
	bearer := lightsailRequest(http.MethodGet, "/api/admin/lightsail/status", "", nil, "")
	bearer.Header.Set("Authorization", "Bearer tenant-api-key")
	if response := serve(handler, bearer); response.Code != http.StatusForbidden {
		t.Fatalf("bearer status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLightsailHTTPCatalogCreateAndStrictBoundaries(t *testing.T) {
	store := testWebStore(t)
	provider := &fakeLightsailCatalogProvider{catalog: lightsailCatalogFixture()}
	handler := NewWithOptions(store, fstest.MapFS{"index.html": {Data: []byte("ok")}}, "http://127.0.0.1:3001", Options{Lightsail: &LightsailOptions{Config: lightsailHTTPConfig(true), Provider: provider}})
	cookie := newLightsailAdminSession(t, store, handler, "lightsail-create@example.com")
	statusResponse := serve(handler, lightsailRequest(http.MethodGet, "/api/admin/lightsail/status", "", cookie, ""))
	if statusResponse.Code != http.StatusOK || statusResponse.Body.String() != "{\"allowedRegions\":[\"us-east-1\"],\"enabled\":true}\n" {
		t.Fatalf("status: %d %s", statusResponse.Code, statusResponse.Body.String())
	}

	catalogResponse := serve(handler, lightsailRequest(http.MethodGet, "/api/admin/lightsail/catalog?region=us-east-1", "", cookie, ""))
	if catalogResponse.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", catalogResponse.Code, catalogResponse.Body.String())
	}
	var catalog lightsail.Catalog
	if err := json.Unmarshal(catalogResponse.Body.Bytes(), &catalog); err != nil || len(catalog.Blueprints) != 1 || catalog.Blueprints[0].ID != "debian" || len(catalog.Bundles) != 1 || catalog.Bundles[0].ID != "small" {
		t.Fatalf("filtered catalog=%+v err=%v", catalog, err)
	}

	body := `{"name":"API node","selection":{"region":"us-east-1","availabilityZone":"us-east-1a","blueprintId":"debian","bundleId":"small"},"script":"echo once\\n"}`
	for name, request := range map[string]*http.Request{
		"missing key":   lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances", body, cookie, ""),
		"unknown field": lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances", strings.TrimSuffix(body, "}")+`,"extra":true}`, cookie, "create-extra"),
		"query":         lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances?x=1", body, cookie, "create-query"),
		"wrong method":  lightsailRequest(http.MethodDelete, "/api/admin/lightsail/instances", body, cookie, "create-delete"),
	} {
		response := serve(handler, request)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: %d %s", name, response.Code, response.Body.String())
		}
	}
	oversized := `{"name":"Large","selection":{"region":"us-east-1","availabilityZone":"us-east-1a","blueprintId":"debian","bundleId":"small"},"script":"` + strings.Repeat("x", 97*1024) + `"}`
	if response := serve(handler, lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances", oversized, cookie, "create-large")); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d %s", response.Code, response.Body.String())
	}

	createdResponse := serve(handler, lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances", body, cookie, "create-api-node"))
	if createdResponse.Code != http.StatusAccepted || strings.Contains(createdResponse.Body.String(), "echo once") || strings.Contains(createdResponse.Body.String(), "script\"") {
		t.Fatalf("create: %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	listResponse := serve(handler, lightsailRequest(http.MethodGet, "/api/admin/lightsail/instances", "", cookie, ""))
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), "echo once") || strings.Contains(listResponse.Body.String(), "script\"") {
		t.Fatalf("list: %d %s", listResponse.Code, listResponse.Body.String())
	}
	emptyBody := `{"name":"Create only","selection":{"region":"us-east-1","availabilityZone":"us-east-1a","blueprintId":"debian","bundleId":"small"},"script":""}`
	emptyResponse := serve(handler, lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances", emptyBody, cookie, "create-without-script"))
	var empty platform.LightsailInstance
	if emptyResponse.Code != http.StatusAccepted || json.Unmarshal(emptyResponse.Body.Bytes(), &empty) != nil || empty.ScriptSize != 0 || empty.DeploymentState != "not-requested" {
		t.Fatalf("empty script create: %d %s", emptyResponse.Code, emptyResponse.Body.String())
	}
}

func TestLightsailHTTPRejectsInvalidCatalogBeforeMutationAndSanitizesProviderErrors(t *testing.T) {
	store := testWebStore(t)
	provider := &fakeLightsailCatalogProvider{catalog: lightsailCatalogFixture()}
	handler := NewWithOptions(store, fstest.MapFS{"index.html": {Data: []byte("ok")}}, "http://127.0.0.1:3001", Options{Lightsail: &LightsailOptions{Config: lightsailHTTPConfig(true), Provider: provider}})
	cookie := newLightsailAdminSession(t, store, handler, "lightsail-validation@example.com")
	invalid := `{"name":"Invalid","selection":{"region":"us-east-1","availabilityZone":"us-east-1a","blueprintId":"missing","bundleId":"small"},"script":"echo once\\n"}`
	response := serve(handler, lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances", invalid, cookie, "invalid-catalog"))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid catalog: %d %s", response.Code, response.Body.String())
	}
	list := serve(handler, lightsailRequest(http.MethodGet, "/api/admin/lightsail/instances", "", cookie, ""))
	if list.Code != http.StatusOK || list.Body.String() != "[]\n" {
		t.Fatalf("invalid catalog mutated state: %d %s", list.Code, list.Body.String())
	}
	provider.err = errors.New("Authorization: Bearer live-provider-secret")
	response = serve(handler, lightsailRequest(http.MethodGet, "/api/admin/lightsail/catalog?region=us-east-1", "", cookie, ""))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "live-provider-secret") {
		t.Fatalf("provider error: %d %s", response.Code, response.Body.String())
	}
}

func TestLightsailHTTPStopAndDestroyUseRevisionAndExactConfirmation(t *testing.T) {
	store := testWebStore(t)
	provider := &fakeLightsailCatalogProvider{catalog: lightsailCatalogFixture()}
	handler := NewWithOptions(store, fstest.MapFS{"index.html": {Data: []byte("ok")}}, "http://127.0.0.1:3001", Options{Lightsail: &LightsailOptions{Config: lightsailHTTPConfig(true), Provider: provider}})
	cookie := newLightsailAdminSession(t, store, handler, "lightsail-actions@example.com")
	body := `{"name":"Action node","selection":{"region":"us-east-1","availabilityZone":"us-east-1a","blueprintId":"debian","bundleId":"small"},"script":""}`
	createdResponse := serve(handler, lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances", body, cookie, "create-action-node"))
	var instance platform.LightsailInstance
	if createdResponse.Code != http.StatusAccepted || json.Unmarshal(createdResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("create: %d %s", createdResponse.Code, createdResponse.Body.String())
	}
	createJob, err := store.ClaimLightsailJob(context.Background(), time.Minute)
	if err != nil || createJob == nil {
		t.Fatalf("claim create: %+v %v", createJob, err)
	}
	arn := "arn:aws:lightsail:us-east-1:123:Instance/test"
	running := "running"
	if err = store.UpdateLightsailInstance(context.Background(), createJob.ID, createJob.LeaseToken, createJob.Stage, platform.LightsailInstanceUpdate{AWSARN: &arn, AWSState: &running}); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteLightsailJob(context.Background(), createJob.ID, createJob.LeaseToken, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	stop := serve(handler, lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances/"+instance.ID+"/stop", `{"revision":2}`, cookie, ""))
	if stop.Code != http.StatusAccepted {
		t.Fatalf("stop: %d %s", stop.Code, stop.Body.String())
	}
	stopJob, err := store.ClaimLightsailJob(context.Background(), time.Minute)
	if err != nil || stopJob == nil || stopJob.Kind != "stop" {
		t.Fatalf("claim stop: %+v %v", stopJob, err)
	}
	stopped := "stopped"
	ip := "203.0.113.18"
	if err = store.UpdateLightsailInstance(context.Background(), stopJob.ID, stopJob.LeaseToken, stopJob.Stage, platform.LightsailInstanceUpdate{AWSState: &stopped, LifecycleState: &stopped, StaticIPAddress: &ip}); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteLightsailJob(context.Background(), stopJob.ID, stopJob.LeaseToken, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	listed := serve(handler, lightsailRequest(http.MethodGet, "/api/admin/lightsail/instances", "", cookie, ""))
	var instances []platform.LightsailInstance
	if err = json.Unmarshal(listed.Body.Bytes(), &instances); err != nil || len(instances) != 1 {
		t.Fatalf("list: %d %s err=%v", listed.Code, listed.Body.String(), err)
	}
	current := instances[0]
	wrong := serve(handler, lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances/"+current.ID+"/destroy", `{"revision":`+jsonNumber(current.Revision)+`,"confirmation":"wrong"}`, cookie, ""))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong confirmation: %d %s", wrong.Code, wrong.Body.String())
	}
	destroy := serve(handler, lightsailRequest(http.MethodPost, "/api/admin/lightsail/instances/"+current.ID+"/destroy", `{"revision":`+jsonNumber(current.Revision)+`,"confirmation":"`+current.AWSInstanceName+`"}`, cookie, ""))
	if destroy.Code != http.StatusAccepted {
		t.Fatalf("destroy: %d %s", destroy.Code, destroy.Body.String())
	}
}

func jsonNumber(value int64) string {
	data, _ := json.Marshal(value)
	return string(data)
}
