package cloud

import (
	"testing"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
)

func TestPasswordAuthOptionsAllowReauth(t *testing.T) {
	options, ok := authOptions(Credentials{
		AuthMethod: "password",
		AuthURL:    "https://iam.example/v3",
		Username:   "user",
		Password:   "password",
	}).(golangsdk.AuthOptions)
	if !ok {
		t.Fatalf("expected AuthOptions, got %T", authOptions(Credentials{AuthMethod: "password"}))
	}
	if !options.AllowReauth {
		t.Fatal("password authentication must allow reauthentication")
	}
}

func TestAKSKAuthOptions(t *testing.T) {
	options, ok := authOptions(Credentials{
		AuthMethod: "aksk",
		AuthURL:    "https://iam.example/v3",
		AccessKey:  "access-key",
		SecretKey:  "secret-key",
		ProjectID:  "project-id",
		Region:     "eu-de",
	}).(golangsdk.AKSKAuthOptions)
	if !ok {
		t.Fatalf("expected AKSKAuthOptions, got %T", authOptions(Credentials{AuthMethod: "aksk"}))
	}
	if options.AccessKey != "access-key" || options.SecretKey != "secret-key" || options.ProjectId != "project-id" || options.Region != "eu-de" {
		t.Fatal("unexpected AK/SK auth options")
	}
}
