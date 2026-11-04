package onboard

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKubernetesGitHubAppReadCredentials(t *testing.T) {
	cases := []struct {
		name       string
		values     string
		fetcher    doctorReadTokenSource
		server     envTokenState
		failChecks int
	}{
		{name: "incomplete Secret reference", values: "fetcher:\n  extraEnv:\n    - name: ASTER_APP_ID\n      value: '123'\n    - name: ASTER_APP_PRIVATE_KEY\n      valueFrom:\n        secretKeyRef:\n          name: app\n", fetcher: readTokenAppInvalid, server: envTokenMissing, failChecks: 1},
		{name: "anonymous", fetcher: readTokenAbsent, server: envTokenMissing},
		{name: "chart App independent of AI", values: "githubApp:\n  existingSecret: github-app\n", fetcher: readTokenAppConfigured, server: envTokenPresent},
		{name: "App wins over static", values: "githubApp:\n  existingSecret: github-app\nai:\n  githubReadTokenSecretName: read-token\n", fetcher: readTokenAppConfigured, server: envTokenPresent},
		{name: "incomplete App wins over static", values: "ai:\n  githubReadTokenSecretName: read-token\nfetcher:\n  extraEnv:\n    - name: ASTER_APP_ID\n      value: '123'\n", fetcher: readTokenAppInvalid, server: envTokenPresent, failChecks: 1},
		{name: "optional App key", values: "fetcher:\n  extraEnv:\n    - name: ASTER_APP_ID\n      value: '123'\n    - name: ASTER_APP_PRIVATE_KEY\n      valueFrom:\n        secretKeyRef:\n          name: app\n          key: pem\n          optional: true\n", fetcher: readTokenAppInvalid, server: envTokenMissing, failChecks: 1},
		{name: "fetcher shadow does not affect server", values: "githubApp:\n  existingSecret: github-app\nfetcher:\n  extraEnv:\n    - name: ASTER_APP_ID\n      value: ''\n", fetcher: readTokenAppInvalid, server: envTokenPresent, failChecks: 1},
		{name: "server shadow does not affect fetcher", values: "githubApp:\n  existingSecret: github-app\nserver:\n  extraEnv:\n    - name: ASTER_APP_PRIVATE_KEY\n      value: ''\n", fetcher: readTokenAppConfigured, server: envTokenInvalid, failChecks: 1},
		{name: "both chart credentials blanked", values: "githubApp:\n  existingSecret: github-app\nfetcher:\n  extraEnv:\n    - name: ASTER_APP_ID\n      value: ''\n    - name: ASTER_APP_PRIVATE_KEY\n      value: ''\n", fetcher: readTokenAppInvalid, server: envTokenPresent, failChecks: 1},
		{name: "optional override retains chart value", values: "githubApp:\n  existingSecret: github-app\nfetcher:\n  extraEnv:\n    - name: ASTER_APP_PRIVATE_KEY\n      valueFrom:\n        secretKeyRef:\n          name: other\n          key: pem\n          optional: true\n", fetcher: readTokenAppConfigured, server: envTokenPresent},
		{name: "missing Secret key name", values: "githubApp:\n  existingSecret: github-app\n  appIDKey: ''\n", fetcher: readTokenAppInvalid, server: envTokenInvalid, failChecks: 2},
		{name: "placeholder Secret name", values: "githubApp:\n  existingSecret: <your-app-secret>\n", fetcher: readTokenAppInvalid, server: envTokenInvalid, failChecks: 2},
		{name: "explicit extra env pair", values: "server:\n  extraEnv:\n    - name: ASTER_APP_ID\n      valueFrom:\n        secretKeyRef:\n          name: app\n          key: id\n    - name: ASTER_APP_PRIVATE_KEY\n      valueFrom:\n        secretKeyRef:\n          name: app\n          key: pem\n", fetcher: readTokenAbsent, server: envTokenPresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var values doctorKubernetesValues
			if err := yaml.Unmarshal([]byte(tc.values), &values); err != nil {
				t.Fatal(err)
			}
			if got := kubernetesReadTokenSource(values); got != tc.fetcher {
				t.Errorf("fetcher credential = %v, want %v", got, tc.fetcher)
			}
			if got := serverReadTokenState(values); got != tc.server {
				t.Errorf("server credential = %v, want %v", got, tc.server)
			}
			var report DoctorReport
			checkKubernetes(&report, []byte(tc.values), nil)
			failures := 0
			for _, check := range report.Checks {
				if strings.HasSuffix(check.Name, "GitHub App") && check.Status == DoctorFail {
					failures++
				}
			}
			if failures != tc.failChecks {
				t.Fatalf("App failures = %d, want %d: %+v", failures, tc.failChecks, report.Checks)
			}
		})
	}
}
