package orka

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const (
	FixServiceAccountNameEnv      = "ORKA_FIX_SERVICE_ACCOUNT_NAME"
	FixServiceAccountNamespaceEnv = "ORKA_FIX_SERVICE_ACCOUNT_NAMESPACE"
	PodNameEnv                    = "POD_NAME"
	PodUIDEnv                     = "POD_UID"

	delegatedTokenLifetime       = 10 * time.Minute
	delegatedTokenRefreshSkew    = time.Minute
	delegatedTokenRequestTimeout = 10 * time.Second
)

var serviceAccountsGVR = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}

type delegatedServiceAccountConfig struct {
	Namespace string
	Name      string
	PodName   string
	PodUID    types.UID
}

func (c delegatedServiceAccountConfig) configured() bool {
	return c.Namespace != "" || c.Name != "" || c.PodName != "" || c.PodUID != ""
}

func validateDelegatedServiceAccountConfig(c delegatedServiceAccountConfig) error {
	if !c.configured() {
		return nil
	}
	c.Namespace = strings.TrimSpace(c.Namespace)
	c.Name = strings.TrimSpace(c.Name)
	c.PodName = strings.TrimSpace(c.PodName)
	c.PodUID = types.UID(strings.TrimSpace(string(c.PodUID)))
	switch {
	case c.Namespace == "":
		return fmt.Errorf("delegated ServiceAccount namespace is required")
	case len(validation.IsDNS1123Label(c.Namespace)) > 0:
		return fmt.Errorf("delegated ServiceAccount namespace is invalid")
	case c.Name == "":
		return fmt.Errorf("delegated ServiceAccount name is required")
	case len(validation.IsDNS1123Subdomain(c.Name)) > 0:
		return fmt.Errorf("delegated ServiceAccount name is invalid")
	case c.PodName == "":
		return fmt.Errorf("bound Pod name is required")
	case len(validation.IsDNS1123Subdomain(c.PodName)) > 0:
		return fmt.Errorf("bound Pod name is invalid")
	case c.PodUID == "":
		return fmt.Errorf("bound Pod UID is required")
	default:
		return nil
	}
}

type serviceAccountTokenRequester interface {
	CreateToken(context.Context, delegatedServiceAccountConfig, int64) (string, time.Time, error)
}

type dynamicServiceAccountTokenRequester struct {
	dyn dynamic.Interface
}

func (r dynamicServiceAccountTokenRequester) CreateToken(
	ctx context.Context,
	config delegatedServiceAccountConfig,
	expirationSeconds int64,
) (string, time.Time, error) {
	if r.dyn == nil {
		return "", time.Time{}, errors.New("kubernetes dynamic client is unavailable")
	}
	request := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenRequest",
		"metadata": map[string]any{
			"name":      config.Name,
			"namespace": config.Namespace,
		},
		"spec": map[string]any{
			"expirationSeconds": expirationSeconds,
			"boundObjectRef": map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"name":       config.PodName,
				"uid":        string(config.PodUID),
			},
		},
	}}
	response, err := r.dyn.Resource(serviceAccountsGVR).Namespace(config.Namespace).
		Create(ctx, request, metav1.CreateOptions{}, "token")
	if err != nil {
		return "", time.Time{}, err
	}
	token, _, _ := unstructured.NestedString(response.Object, "status", "token")
	expiration, _, _ := unstructured.NestedString(response.Object, "status", "expirationTimestamp")
	expiresAt, err := time.Parse(time.RFC3339Nano, expiration)
	if err != nil {
		return "", time.Time{}, errors.New("delegated ServiceAccount token expiration is invalid")
	}
	return token, expiresAt, nil
}

type boundServiceAccountTokenSource struct {
	mu        sync.Mutex
	requester serviceAccountTokenRequester
	config    delegatedServiceAccountConfig
	now       func() time.Time
	token     string
	expiresAt time.Time
}

func (s *boundServiceAccountTokenSource) Token() (string, error) {
	if s == nil || s.requester == nil {
		return "", errors.New("delegated ServiceAccount token source is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	if s.token != "" && now.Add(delegatedTokenRefreshSkew).Before(s.expiresAt) {
		return s.token, nil
	}
	expirationSeconds := int64(delegatedTokenLifetime / time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), delegatedTokenRequestTimeout)
	defer cancel()
	token, expiresAt, err := s.requester.CreateToken(ctx, s.config, expirationSeconds)
	if err != nil {
		return "", fmt.Errorf("request delegated ServiceAccount token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", errors.New("delegated ServiceAccount token response is empty")
	}
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(now) {
		return "", errors.New("delegated ServiceAccount token is already expired")
	}
	s.token = strings.TrimSpace(token)
	s.expiresAt = expiresAt
	return s.token, nil
}

func (s *boundServiceAccountTokenSource) invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.token = ""
	s.expiresAt = time.Time{}
	s.mu.Unlock()
}

type delegatedBearerRoundTripper struct {
	base   http.RoundTripper
	tokens *boundServiceAccountTokenSource
}

func (t *delegatedBearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.base == nil || t.tokens == nil {
		return nil, errors.New("delegated Kubernetes transport is unavailable")
	}
	token, err := t.tokens.Token()
	if err != nil {
		return nil, err
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+token)
	response, err := t.base.RoundTrip(clone)
	if err == nil && response != nil && response.StatusCode == http.StatusUnauthorized {
		t.tokens.invalidate()
	}
	return response, err
}

func newDelegatedServiceAccountClients(
	base *rest.Config,
	config delegatedServiceAccountConfig,
) (*rest.Config, resultTokenSource, error) {
	if base == nil {
		return nil, nil, errors.New("kubernetes REST config is required")
	}
	if err := validateDelegatedServiceAccountConfig(config); err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(base)
	if err != nil {
		return nil, nil, fmt.Errorf("create delegated ServiceAccount token client: %w", err)
	}
	config.Namespace = strings.TrimSpace(config.Namespace)
	config.Name = strings.TrimSpace(config.Name)
	config.PodName = strings.TrimSpace(config.PodName)
	config.PodUID = types.UID(strings.TrimSpace(string(config.PodUID)))
	tokens := &boundServiceAccountTokenSource{
		requester: dynamicServiceAccountTokenRequester{dyn: dyn},
		config:    config,
		now:       time.Now,
	}
	delegated := rest.AnonymousClientConfig(base)
	delegated.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
		return &delegatedBearerRoundTripper{base: base, tokens: tokens}
	}
	return delegated, tokens, nil
}
