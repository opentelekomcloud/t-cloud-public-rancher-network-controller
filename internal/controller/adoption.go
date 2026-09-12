package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/opentelekomcloud/t-cloud-public-rancher-network-controller/api/v1alpha1"
)

const maxMachineConfigSize = 2 << 20

type machineDriverState struct {
	DriverName string `json:"DriverName"`
	Driver     struct {
		VPCID                managedResourceID `json:"vpc_id"`
		SubnetID             managedResourceID `json:"subnet_id"`
		ManagedSecurityGroup string            `json:"managed_security_group"`
	} `json:"Driver"`
}

type managedResourceID struct {
	Value   string `json:"value"`
	Managed bool   `json:"managed"`
}

func (r *TCloudClusterNetworkReconciler) discoverAdoptableResources(ctx context.Context, network *infrav1.TCloudClusterNetwork) (infrav1.NetworkResourceStatus, error) {
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(tcloudMachineGVK.GroupVersion().WithKind(tcloudMachineGVK.Kind + "List"))
	if err := r.List(ctx, machines, client.InNamespace(network.Spec.ClusterRef.Namespace), client.MatchingLabels{clusterNameLabel: network.Spec.ClusterRef.Name}); err != nil {
		return infrav1.NetworkResourceStatus{}, fmt.Errorf("list T-Cloud machines: %w", err)
	}

	candidates := make([]unstructured.Unstructured, 0, len(machines.Items))
	for i := range machines.Items {
		if machines.Items[i].GetDeletionTimestamp() == nil {
			candidates = append(candidates, machines.Items[i])
		}
	}
	if len(candidates) == 0 {
		return infrav1.NetworkResourceStatus{}, fmt.Errorf("no T-Cloud machine is available for network adoption")
	}

	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i].GetCreationTimestamp(), candidates[j].GetCreationTimestamp()
		if left.Time.Equal(right.Time) {
			return candidates[i].GetName() < candidates[j].GetName()
		}
		return left.Before(&right)
	})

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	for i := range candidates {
		stateSecret := &corev1.Secret{}
		secretName := candidates[i].GetName() + "-machine-state"
		if err := reader.Get(ctx, types.NamespacedName{Namespace: network.Spec.ClusterRef.Namespace, Name: secretName}, stateSecret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return infrav1.NetworkResourceStatus{}, fmt.Errorf("read machine state Secret %s/%s: %w", network.Spec.ClusterRef.Namespace, secretName, err)
		}

		state, err := decodeMachineDriverState(stateSecret.Data["extractedConfig"])
		if err != nil {
			return infrav1.NetworkResourceStatus{}, fmt.Errorf("decode machine state Secret %s/%s: %w", network.Spec.ClusterRef.Namespace, secretName, err)
		}
		return adoptableResourcesFromState(candidates[i].GetName(), state)
	}
	return infrav1.NetworkResourceStatus{}, fmt.Errorf("no T-Cloud machine with complete driver state is available for network adoption")
}

func adoptableResourcesFromState(machineName string, state machineDriverState) (infrav1.NetworkResourceStatus, error) {
	if state.DriverName != "opentelekomcloud" {
		return infrav1.NetworkResourceStatus{}, fmt.Errorf("machine %s uses driver %q, not opentelekomcloud", machineName, state.DriverName)
	}
	if !state.Driver.VPCID.Managed || !state.Driver.SubnetID.Managed || state.Driver.ManagedSecurityGroup == "" {
		return infrav1.NetworkResourceStatus{}, fmt.Errorf("machine %s does not own a complete driver-managed network; select Existing network ownership instead", machineName)
	}
	if state.Driver.VPCID.Value == "" || state.Driver.SubnetID.Value == "" {
		return infrav1.NetworkResourceStatus{}, fmt.Errorf("machine %s state does not contain complete network resource IDs", machineName)
	}

	return infrav1.NetworkResourceStatus{
		VPC:           infrav1.ResourceStatus{ID: state.Driver.VPCID.Value, ControllerManaged: true},
		Subnet:        infrav1.ResourceStatus{ID: state.Driver.SubnetID.Value, ControllerManaged: true},
		SecurityGroup: infrav1.ResourceStatus{ID: state.Driver.ManagedSecurityGroup, ControllerManaged: true},
	}, nil
}

func decodeMachineDriverState(value []byte) (machineDriverState, error) {
	if len(value) == 0 {
		return machineDriverState{}, fmt.Errorf("extractedConfig is empty")
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(value))
	if err != nil {
		return machineDriverState{}, fmt.Errorf("open compressed machine state: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return machineDriverState{}, fmt.Errorf("read machine state archive: %w", err)
		}
		if filepath.Base(header.Name) != "config.json" {
			continue
		}
		if header.Size < 1 || header.Size > maxMachineConfigSize {
			return machineDriverState{}, fmt.Errorf("machine config size %d is invalid", header.Size)
		}
		state := machineDriverState{}
		if err := json.NewDecoder(io.LimitReader(tarReader, maxMachineConfigSize)).Decode(&state); err != nil {
			return machineDriverState{}, fmt.Errorf("parse machine config: %w", err)
		}
		return state, nil
	}
	return machineDriverState{}, fmt.Errorf("config.json is missing from extractedConfig")
}
