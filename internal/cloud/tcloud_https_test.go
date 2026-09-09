//go:build integration

package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	golangsdk "github.com/opentelekomcloud/gophertelekomcloud"
)

func TestTCloudServiceVPCOverHTTPS(t *testing.T) {
	t.Helper()

	const (
		projectID = "project-id"
		vpcID     = "vpc-id"
		vpcName   = "cluster-network-uid"
	)

	var mu sync.Mutex
	created := false
	deleted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != "test-token" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/"+projectID+"/vpcs":
			if created && !deleted {
				_, _ = w.Write([]byte(`{"vpcs":[{"id":"vpc-id","name":"cluster-network-uid","cidr":"192.168.0.0/16","status":"OK"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"vpcs":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/"+projectID+"/vpcs":
			var body struct {
				VPC struct {
					Name string `json:"name"`
					CIDR string `json:"cidr"`
				} `json:"vpc"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if body.VPC.Name != vpcName || body.VPC.CIDR != "192.168.0.0/16" {
				http.Error(w, "unexpected create body", http.StatusBadRequest)
				return
			}
			created = true
			_, _ = w.Write([]byte(`{"vpc":{"id":"vpc-id","name":"cluster-network-uid","cidr":"192.168.0.0/16","status":"CREATING"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/"+projectID+"/vpcs/"+vpcID:
			if deleted {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	provider := &golangsdk.ProviderClient{TokenID: "test-token", HTTPClient: *server.Client(), ProjectID: projectID}
	client := &golangsdk.ServiceClient{ProviderClient: provider, Endpoint: server.URL + "/"}
	service := &tcloudService{vpc: client, compute: client, network: client}

	request := VPCRequest{Name: vpcName, CIDR: "192.168.0.0/16"}
	first, err := service.EnsureVPC(context.Background(), request)
	if err != nil {
		t.Fatalf("create VPC over HTTPS: %v", err)
	}
	if first.ID != vpcID || first.Name != vpcName {
		t.Fatalf("unexpected created VPC: %#v", first)
	}
	second, err := service.EnsureVPC(context.Background(), request)
	if err != nil {
		t.Fatalf("find VPC over HTTPS: %v", err)
	}
	if second != first {
		t.Fatalf("idempotent lookup returned %#v, want %#v", second, first)
	}
	if err := service.DeleteVPC(context.Background(), vpcID); err != nil {
		t.Fatalf("delete VPC over HTTPS: %v", err)
	}
	if err := service.DeleteVPC(context.Background(), vpcID); err != nil {
		t.Fatalf("repeated delete should accept 404: %v", err)
	}
}
