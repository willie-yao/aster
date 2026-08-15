package orka

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

type serviceAccountTokenResponse struct {
	token     string
	expiresAt time.Time
}

type fakeServiceAccountTokenRequester struct {
	calls             int
	config            delegatedServiceAccountConfig
	expirationSeconds int64
	responses         []serviceAccountTokenResponse
	err               error
}

func (f *fakeServiceAccountTokenRequester) CreateToken(
	_ context.Context,
	config delegatedServiceAccountConfig,
	expirationSeconds int64,
) (string, time.Time, error) {
	f.calls++
	f.config = config
	f.expirationSeconds = expirationSeconds
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	if len(f.responses) == 0 {
		return "", time.Time{}, nil
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	return response.token, response.expiresAt, nil
}

func TestBoundServiceAccountTokenSourceCachesAndRefreshes(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	requester := &fakeServiceAccountTokenRequester{responses: []serviceAccountTokenResponse{
		{token: "token-one", expiresAt: now.Add(10 * time.Minute)},
		{token: "token-two", expiresAt: now.Add(20 * time.Minute)},
	}}
	source := &boundServiceAccountTokenSource{
		requester: requester,
		config: delegatedServiceAccountConfig{
			Namespace: "dashboard", Name: "dashboard-fix", PodName: "dashboard-server-abc", PodUID: types.UID("pod-uid"),
		},
		now: func() time.Time { return now },
	}
	first, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if first != "token-one" || second != first || requester.calls != 1 {
		t.Fatalf("tokens=%q,%q calls=%d", first, second, requester.calls)
	}
	if requester.expirationSeconds != 600 || requester.config.Namespace != "dashboard" || requester.config.Name != "dashboard-fix" || requester.config.PodName != "dashboard-server-abc" || requester.config.PodUID != types.UID("pod-uid") {
		t.Fatalf("request config=%+v expiration=%d", requester.config, requester.expirationSeconds)
	}
	now = now.Add(9*time.Minute + time.Second)
	refreshed, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != "token-two" || requester.calls != 2 {
		t.Fatalf("refreshed=%q calls=%d", refreshed, requester.calls)
	}
}

func TestBoundServiceAccountTokenSourceFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		requester serviceAccountTokenRequester
		want      string
	}{
		{name: "missing requester", want: "unavailable"},
		{name: "request error", requester: &fakeServiceAccountTokenRequester{err: errors.New("denied")}, want: "request delegated"},
		{name: "empty response", requester: &fakeServiceAccountTokenRequester{}, want: "response is empty"},
		{name: "expired", requester: &fakeServiceAccountTokenRequester{responses: []serviceAccountTokenResponse{{token: "old", expiresAt: now}}}, want: "already expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &boundServiceAccountTokenSource{
				requester: tc.requester,
				config:    delegatedServiceAccountConfig{Name: "fix", PodName: "pod", PodUID: types.UID("uid")},
				now:       func() time.Time { return now },
			}
			if _, err := source.Token(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateDelegatedServiceAccountConfig(t *testing.T) {
	valid := delegatedServiceAccountConfig{Namespace: "dashboard", Name: "dashboard-fix", PodName: "dashboard-server-abc", PodUID: types.UID("uid")}
	for _, tc := range []struct {
		name   string
		config delegatedServiceAccountConfig
		want   string
	}{
		{name: "disabled", config: delegatedServiceAccountConfig{}},
		{name: "valid", config: valid},
		{name: "namespace", config: delegatedServiceAccountConfig{Name: valid.Name, PodName: valid.PodName, PodUID: valid.PodUID}, want: "namespace is required"},
		{name: "name", config: delegatedServiceAccountConfig{Namespace: valid.Namespace, PodName: valid.PodName, PodUID: valid.PodUID}, want: "name is required"},
		{name: "pod name", config: delegatedServiceAccountConfig{Namespace: valid.Namespace, Name: valid.Name, PodUID: valid.PodUID}, want: "Pod name is required"},
		{name: "pod uid", config: delegatedServiceAccountConfig{Namespace: valid.Namespace, Name: valid.Name, PodName: valid.PodName}, want: "Pod UID is required"},
		{name: "invalid service account", config: delegatedServiceAccountConfig{Namespace: valid.Namespace, Name: "Bad_Name", PodName: valid.PodName, PodUID: valid.PodUID}, want: "name is invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDelegatedServiceAccountConfig(tc.config)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDelegatedBearerRoundTripperUsesOnlyDelegatedToken(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	requester := &fakeServiceAccountTokenRequester{responses: []serviceAccountTokenResponse{
		{token: "delegated-token", expiresAt: now.Add(10 * time.Minute)},
		{token: "refreshed-token", expiresAt: now.Add(20 * time.Minute)},
	}}
	source := &boundServiceAccountTokenSource{
		requester: requester,
		config:    delegatedServiceAccountConfig{Name: "fix", PodName: "pod", PodUID: types.UID("uid")},
		now:       func() time.Time { return now },
	}
	var got []string
	transport := &delegatedBearerRoundTripper{
		tokens: source,
		base: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			got = append(got, request.Header.Get("Authorization"))
			status := http.StatusOK
			if len(got) == 1 {
				status = http.StatusUnauthorized
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodGet, "https://kubernetes.example.test/apis", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer pod-token")
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "Bearer delegated-token" || got[1] != "Bearer refreshed-token" || requester.calls != 2 {
		t.Fatalf("authorization=%v calls=%d", got, requester.calls)
	}
	if request.Header.Get("Authorization") != "Bearer pod-token" {
		t.Fatalf("original request was mutated: %q", request.Header.Get("Authorization"))
	}
}

func TestResultClientInvalidatesRejectedDelegatedToken(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	requester := &fakeServiceAccountTokenRequester{responses: []serviceAccountTokenResponse{
		{token: "token-one", expiresAt: now.Add(10 * time.Minute)},
		{token: "token-two", expiresAt: now.Add(20 * time.Minute)},
	}}
	source := &boundServiceAccountTokenSource{
		requester: requester,
		config:    delegatedServiceAccountConfig{Name: "fix", PodName: "pod", PodUID: types.UID("uid")},
		now:       func() time.Time { return now },
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls++
		if calls == 1 {
			if got := request.Header.Get("Authorization"); got != "Bearer token-one" {
				t.Errorf("first authorization = %q", got)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer token-two" {
			t.Errorf("second authorization = %q", got)
		}
		_, _ = io.WriteString(w, `{"result":"ok"}`)
	}))
	defer server.Close()
	client := newResultClient(server.URL, source)
	if _, _, err := client.Result(t.Context(), "orka-system", "task"); !IsResultAuthorizationError(err) {
		t.Fatalf("first result error = %v", err)
	}
	result, ok, err := client.Result(t.Context(), "orka-system", "task")
	if err != nil || !ok || result != "ok" || requester.calls != 2 {
		t.Fatalf("result=%q ok=%v error=%v token_calls=%d", result, ok, err, requester.calls)
	}
}

func TestDelegatedServiceAccountClientUsesTokenSubresource(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer source-token" {
			t.Errorf("token request authorization = %q", got)
		}
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/namespaces/dashboard/serviceaccounts/dashboard-fix/token" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "authentication.k8s.io/v1",
			"kind":       "TokenRequest",
			"status": map[string]any{
				"token":               "delegated-token",
				"expirationTimestamp": now.Add(10 * time.Minute).Format(time.RFC3339Nano),
			},
		})
	}))
	defer server.Close()
	delegated, tokens, err := newDelegatedServiceAccountClients(&rest.Config{Host: server.URL, BearerToken: "source-token"}, delegatedServiceAccountConfig{
		Namespace: "dashboard", Name: "dashboard-fix", PodName: "dashboard-server-abc", PodUID: types.UID("pod-uid"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if delegated.BearerToken != "" || delegated.BearerTokenFile != "" || delegated.WrapTransport == nil {
		t.Fatalf("delegated REST credentials were not isolated: %+v", delegated)
	}
	source := tokens.(*boundServiceAccountTokenSource)
	source.now = func() time.Time { return now }
	if token, err := source.Token(); err != nil || token != "delegated-token" {
		t.Fatalf("token=%q error=%v", token, err)
	}
	spec, _ := body["spec"].(map[string]any)
	bound, _ := spec["boundObjectRef"].(map[string]any)
	if spec["expirationSeconds"] != float64(600) || bound["apiVersion"] != "v1" || bound["kind"] != "Pod" || bound["name"] != "dashboard-server-abc" || bound["uid"] != "pod-uid" {
		t.Fatalf("token request body = %#v", body)
	}
}
