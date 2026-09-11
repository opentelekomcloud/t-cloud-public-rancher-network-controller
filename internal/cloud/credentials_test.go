package cloud

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCredentialsFromSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "credential", Namespace: "cattle-global-data"},
		Data: map[string][]byte{
			credentialPrefix + "authUrl":     []byte("https://iam.example/v3"),
			credentialPrefix + "username":    []byte("user"),
			credentialPrefix + "password":    []byte("secret-value"),
			credentialPrefix + "domainName":  []byte("domain"),
			credentialPrefix + "projectName": []byte("secret-project"),
			credentialPrefix + "region":      []byte("secret-region"),
		},
	}

	credentials, err := CredentialsFromSecret(secret, "spec-region", "spec-project", "internal")
	if err != nil {
		t.Fatalf("CredentialsFromSecret() error = %v", err)
	}
	if credentials.Region != "spec-region" || credentials.ProjectName != "spec-project" {
		t.Fatal("spec values did not override secret values")
	}
	if credentials.Password != "secret-value" || credentials.EndpointType != "internal" {
		t.Fatal("unexpected username/password credentials")
	}
	if credentials.AuthMethod != "password" {
		t.Fatalf("unexpected authentication method: %q", credentials.AuthMethod)
	}
}

func TestCredentialsFromSecretAKSK(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "credential", Namespace: "cattle-global-data"},
		Data: map[string][]byte{
			credentialPrefix + "authMethod": []byte("aksk"),
			credentialPrefix + "authUrl":    []byte("https://iam.example/v3"),
			credentialPrefix + "accessKey":  []byte("access-key"),
			credentialPrefix + "secretKey":  []byte("secret-key"),
			credentialPrefix + "domainName": []byte("domain"),
			credentialPrefix + "projectId":  []byte("project-id"),
			credentialPrefix + "region":     []byte("eu-de"),
		},
	}

	credentials, err := CredentialsFromSecret(secret, "", "", "")
	if err != nil {
		t.Fatalf("CredentialsFromSecret() error = %v", err)
	}
	if credentials.AuthMethod != "aksk" || credentials.AccessKey != "access-key" || credentials.SecretKey != "secret-key" {
		t.Fatal("unexpected AK/SK credentials")
	}
}

func TestCredentialsFromSecretInfersAKSK(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "credential", Namespace: "cattle-global-data"},
		Data: map[string][]byte{
			credentialPrefix + "authUrl":    []byte("https://iam.example/v3"),
			credentialPrefix + "accessKey":  []byte("access-key"),
			credentialPrefix + "secretKey":  []byte("secret-key"),
			credentialPrefix + "domainName": []byte("domain"),
			credentialPrefix + "projectId":  []byte("project-id"),
			credentialPrefix + "region":     []byte("eu-de"),
		},
	}

	credentials, err := CredentialsFromSecret(secret, "", "", "")
	if err != nil {
		t.Fatalf("CredentialsFromSecret() error = %v", err)
	}
	if credentials.AuthMethod != "aksk" {
		t.Fatalf("unexpected authentication method: %q", credentials.AuthMethod)
	}
}

func TestCredentialsErrorDoesNotExposePassword(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "credential", Namespace: "cattle-global-data"},
		Data:       map[string][]byte{credentialPrefix + "password": []byte("do-not-log")},
	}

	_, err := CredentialsFromSecret(secret, "", "", "")
	if err == nil {
		t.Fatal("expected invalid credentials error")
	}
	if strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("error exposed the password: %v", err)
	}
}

func TestCredentialsErrorDoesNotExposeSecretKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "credential", Namespace: "cattle-global-data"},
		Data: map[string][]byte{
			credentialPrefix + "authMethod": []byte("aksk"),
			credentialPrefix + "secretKey":  []byte("do-not-log"),
		},
	}

	_, err := CredentialsFromSecret(secret, "", "", "")
	if err == nil {
		t.Fatal("expected invalid credentials error")
	}
	if strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("error exposed the secret key: %v", err)
	}
}
