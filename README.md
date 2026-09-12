# T-Cloud Rancher Network Controller

Kubernetes controller that owns shared T-Cloud VPC, subnet, and security-group
resources for Rancher-provisioned RKE2 clusters.

The controller runs in Rancher's management cluster. Machine drivers continue
to own machine-scoped resources such as instances and EIPs; this controller owns
only resources represented by a controller-owned `TCloudClusterNetwork`.

## Policies

- `Managed` creates shared resources and deletes them when the network object is
  deleted.
- `Adopt` reads the oldest machine with complete Rancher machine state, verifies
  that its VPC, subnet, and security group were created by the machine driver,
  adds the requested CNI rules, and takes responsibility for deleting them.
- `Observe` validates existing resources and never deletes them.
- `Abandon` is an administrator recovery option that releases the finalizer
  without deleting cloud resources.

Managed resources receive a name suffix derived from the Kubernetes object's
UID. Cleanup verifies the stored ID, expected name, and `controllerManaged`
status before issuing a delete operation.

## Scaling behavior

New clusters should select Managed or Existing shared networking even when they
start with one node. For a legacy single-node T-Cloud cluster that still has a
driver-managed network, the bootstrap reconciler creates an internal `Adopt`
request as soon as the first machine state is complete. After adoption is ready,
it patches every referenced `OpentelekomcloudConfig` to `networkScope: shared`
with the adopted VPC, subnet, and security-group values. It also maintains the
cluster network, ownership, and `ui.rancher/provider: opentelekomcloud`
annotations.

Because this convergence happens before a later scale operation, changing pool
quantity through Rancher's `+1` control, editing the cluster form, or updating
the provisioning Cluster manifest all use the same shared network. Newly
referenced T-Cloud pool configs are converged to that network as well. Explicit
Existing network IDs are never automatically claimed; use the `Observe` policy
for those resources.

For Managed and Adopt networks, `spec.network.securityGroup.sshAllowedCIDRs` is
editable after cluster creation. CIDRs must be valid and unique. Reconciliation
adds new SSH rules first, then removes only rules found in the controller's last
successfully applied CIDR set. User-created rules are left intact, and Observe
networks are never pruned.

## Development

```bash
make all
make test-race
make manifests generate
```

Build the image:

```bash
make docker-build IMG=ghcr.io/opentelekomcloud/t-cloud-public-rancher-network-controller:dev
```

## Installation

For the recommended installation from Rancher's Apps UI, including release,
verification, upgrade, and safe-uninstall steps, see
[Install through the Rancher UI](docs/install-rancher-ui.md).

With Kustomize:

```bash
kubectl apply -k config
```

With Helm:

```bash
helm upgrade --install t-cloud-network-controller \
  charts/t-cloud-network-controller \
  --namespace cattle-tcloud-system \
  --create-namespace
```

Install the controller before enabling managed networking in the Rancher UI
extension. See `config/samples` for Managed, Adopt, and Observe examples.

### Upgrade

Helm does not upgrade files from a chart's `crds/` directory. Apply the new CRD
manifest first, then upgrade the controller:

```bash
kubectl apply -f charts/t-cloud-network-controller/crds/infrastructure.otc.t-systems.com_tcloudclusternetworks.yaml
helm upgrade t-cloud-network-controller \
  charts/t-cloud-network-controller \
  --namespace cattle-tcloud-system \
  --set image.repository=REGISTRY/t-cloud-public-rancher-network-controller \
  --set image.tag=VERSION
```

Only one replica is active at a time because leader election is enabled. Keep
the CRD installed while any `TCloudClusterNetwork` objects exist.

### Uninstall

Delete Rancher clusters and wait until their `TCloudClusterNetwork` resources
and finalizers are gone before removing the controller. Then uninstall the
workload and, only after confirming no network objects remain, delete the CRD:

```bash
kubectl get tcloudclusternetworks.infrastructure.otc.t-systems.com -A
helm uninstall t-cloud-network-controller --namespace cattle-tcloud-system
kubectl delete crd tcloudclusternetworks.infrastructure.otc.t-systems.com
```

Removing the controller or CRD first prevents managed cloud-network cleanup.

## Deletion and recovery

During deletion the controller waits for Rancher T-Cloud machine objects and
attached cloud ports, then deletes the security group, subnet, and VPC in that
order. A missing resource is treated as already deleted.

If credentials are permanently unavailable, an administrator may patch a
deleting object to `spec.managementPolicy: Abandon`. This intentionally leaves
cloud resources behind and removes the finalizer on the next reconciliation.

Do not manually remove
`infrastructure.otc.t-systems.com/network-cleanup` unless the remaining cloud
resources have been inspected and accepted as orphaned.

Before rollback, keep the CRD and run the previous compatible controller image.
If cleanup cannot be restored, switch an affected object to `Abandon`, record
the VPC/subnet/security-group IDs from status, and arrange manual cleanup.

## Security

The controller reads referenced Rancher cloud-credential and machine-state
Secrets through an uncached API reader. Adoption decodes only the network
ownership fields from `config.json`; credentials and private keys are never
stored in CR status, logs, or Kubernetes events. The ServiceAccount only has
`get` access to Secrets.

## Optional live smoke test

This test creates billable cloud resources. Use a disposable project and a
dedicated Rancher cloud credential:

1. Install the controller and confirm its Deployment is available.
2. Apply a copy of `config/samples/infrastructure_v1alpha1_tcloudclusternetwork_managed.yaml`
   with unique CIDRs, names, cluster reference, and credential Secret reference.
3. Wait for `Ready=True` and verify the three IDs in `.status.resources`.
4. Delete the sample and verify, in order, that its security group, subnet, and
   VPC disappear and that the Kubernetes object finalizes.
5. Repeat with `managementPolicy: Observe`; deleting that object must leave all
   referenced cloud resources intact.

Never use `Abandon` as the normal uninstall path: it deliberately leaves cloud
resources behind.
