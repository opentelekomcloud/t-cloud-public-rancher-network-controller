package cloud

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const credentialPrefix = "opentelekomcloudcredentialConfig-"

func CredentialsFromSecret(secret *corev1.Secret, fallbackRegion, fallbackProject, fallbackEndpointType string) (Credentials, error) {
	value := func(key string) string {
		return strings.TrimSpace(string(secret.Data[credentialPrefix+key]))
	}

	credentials := Credentials{
		AuthURL:      value("authUrl"),
		AuthMethod:   strings.ToLower(value("authMethod")),
		Username:     value("username"),
		Password:     string(secret.Data[credentialPrefix+"password"]),
		AccessKey:    value("accessKey"),
		SecretKey:    string(secret.Data[credentialPrefix+"secretKey"]),
		DomainName:   value("domainName"),
		DomainID:     value("domainId"),
		ProjectName:  firstNonEmpty(fallbackProject, value("projectName")),
		ProjectID:    value("projectId"),
		Region:       firstNonEmpty(fallbackRegion, value("region")),
		EndpointType: firstNonEmpty(fallbackEndpointType, value("endpointType"), "public"),
	}

	if credentials.AuthURL == "" {
		return Credentials{}, fmt.Errorf("cloud credential secret %s/%s has no authUrl", secret.Namespace, secret.Name)
	}
	if credentials.AuthMethod == "" {
		if credentials.AccessKey != "" || credentials.SecretKey != "" {
			credentials.AuthMethod = "aksk"
		} else {
			credentials.AuthMethod = "password"
		}
	}
	switch credentials.AuthMethod {
	case "password":
		if credentials.Username == "" || credentials.Password == "" {
			return Credentials{}, fmt.Errorf("cloud credential secret %s/%s has incomplete username/password authentication", secret.Namespace, secret.Name)
		}
	case "aksk":
		if credentials.AccessKey == "" || credentials.SecretKey == "" {
			return Credentials{}, fmt.Errorf("cloud credential secret %s/%s has incomplete AK/SK authentication", secret.Namespace, secret.Name)
		}
	default:
		return Credentials{}, fmt.Errorf("cloud credential secret %s/%s has unsupported authentication method %q", secret.Namespace, secret.Name, credentials.AuthMethod)
	}
	if credentials.DomainName == "" && credentials.DomainID == "" {
		return Credentials{}, fmt.Errorf("cloud credential secret %s/%s has no domain", secret.Namespace, secret.Name)
	}
	if credentials.Region == "" {
		return Credentials{}, fmt.Errorf("cloud credential secret %s/%s has no region", secret.Namespace, secret.Name)
	}
	if credentials.ProjectName == "" && credentials.ProjectID == "" {
		return Credentials{}, fmt.Errorf("cloud credential secret %s/%s has no project", secret.Namespace, secret.Name)
	}

	return credentials, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
