package onboard

import "strings"

type doctorGitHubAppValues struct {
	ExistingSecret string  `yaml:"existingSecret"`
	AppIDKey       *string `yaml:"appIDKey"`
	PrivateKeyKey  *string `yaml:"privateKeyKey"`
}

type doctorAppState int

const (
	appAbsent doctorAppState = iota
	appConfigured
	appInvalid
)

// kubernetesAppState resolves both App variables after workload overrides.
func kubernetesAppState(values doctorKubernetesValues, extra []doctorExtraEnv) (doctorAppState, string) {
	appID, key := envTokenMissing, envTokenMissing
	chartConfigured := values.GitHubApp.ExistingSecret != ""
	if chartConfigured {
		if placeholder(values.GitHubApp.ExistingSecret) {
			return appInvalid, "githubApp.existingSecret is a placeholder"
		}
		for _, name := range []*string{values.GitHubApp.AppIDKey, values.GitHubApp.PrivateKeyKey} {
			if name != nil && strings.TrimSpace(*name) == "" {
				return appInvalid, "githubApp Secret key names must not be empty"
			}
		}
		appID, key = envTokenPresent, envTokenPresent
	}
	for _, env := range extra {
		name := strings.TrimSpace(env.Name)
		if name != "ASTER_APP_ID" && name != "ASTER_APP_PRIVATE_KEY" {
			continue
		}
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
			ref := env.ValueFrom.SecretKeyRef
			if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Key) == "" {
				return appInvalid, "GitHub App Secret references require a name and key"
			}
		}
		switch name {
		case "ASTER_APP_ID":
			appID = env.fold(appID)
		case "ASTER_APP_PRIVATE_KEY":
			key = env.fold(key)
		}
	}
	switch {
	case appID == envTokenPresent && key == envTokenPresent:
		return appConfigured, "both required GitHub App credential references are configured"
	case appID == envTokenOptional || key == envTokenOptional:
		return appInvalid, "an optional GitHub App Secret key may leave authentication incomplete or absent"
	case chartConfigured || appID != envTokenMissing || key != envTokenMissing:
		return appInvalid, "GitHub App credentials are incomplete or blanked by extraEnv"
	default:
		return appAbsent, ""
	}
}

func checkKubernetesGitHubApp(add func(string, DoctorStatus, string, string), workload string, values doctorKubernetesValues, extra []doctorExtraEnv) {
	state, detail := kubernetesAppState(values, extra)
	switch state {
	case appConfigured:
		add("Kubernetes "+workload+" GitHub App", DoctorPass, detail,
			"Verify installation on every read repository, including kubernetes/test-infra, with Contents read and Pull requests read where triage or escalation is enabled. Installation and permissions are not verified offline.")
	case appInvalid:
		add("Kubernetes "+workload+" GitHub App", DoctorFail, detail,
			"Supply both ASTER_APP_ID and ASTER_APP_PRIVATE_KEY through required Secret references and remove conflicting extraEnv entries. App authentication takes precedence over static tokens and never falls back on failure.")
	}
}
