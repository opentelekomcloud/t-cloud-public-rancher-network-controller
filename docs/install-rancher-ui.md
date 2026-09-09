# Install through the Rancher UI

Install this controller into Rancher's **local management cluster**, not into a
downstream workload cluster. It watches Rancher provisioning Clusters, machine
objects, and cloud-credential Secrets that exist in the local cluster.

## Prerequisites

- Rancher 2.9 or newer for OCI chart repositories.
- A published controller release with a semantic tag such as `v0.1.0`.
- Public access to both GHCR packages, or equivalent registry credentials.
- A T-Cloud cloud credential configured in Rancher.

The release workflow publishes:

- controller image: `ghcr.io/opentelekomcloud/t-cloud-public-rancher-network-controller:<version>`;
- Helm chart: `oci://ghcr.io/opentelekomcloud/charts/t-cloud-network-controller:<version>`;
- packaged chart: `t-cloud-network-controller-<version>.tgz` attached to the
  GitHub release.

GitHub creates new GHCR packages as private unless the organization configures
different defaults. For installation without registry credentials, open each
package under the GitHub organization and change its visibility to **Public**.

## Publish a release

The `Release` GitHub Actions workflow must already exist on the repository's
default branch.

1. Open the repository on GitHub and select **Releases → Draft a new release**.
2. Select **Choose a tag → Create new tag**, using `vMAJOR.MINOR.PATCH`, for
   example `v0.1.0`.
3. Target the commit that should be released and complete the release notes.
4. Save the draft if it needs review.
5. Select **Publish release** when it is ready.
6. Open **Actions → Release** and wait for the `publish` job to succeed.

GitHub does not start release workflows when a draft is merely created or
edited. Publishing the prepared draft triggers the workflow for its tag. Both
stable releases and prereleases use the `published` event.

## Add the chart repository

1. In Rancher, open **Cluster Management**.
2. Find the `local` cluster and select **Explore**.
3. Open **Apps → Repositories** and select **Create**.
4. Enter the name `t-cloud-network-controller`.
5. Select **OCI Repository** as the target.
6. Enter this OCI URL:

   ```text
   oci://ghcr.io/opentelekomcloud/charts/t-cloud-network-controller
   ```

7. For public packages, leave authentication empty. For private packages,
   configure Basic Auth with a GitHub username and a token that has
   `read:packages`; the controller image also needs an image pull Secret.
8. Select **Create** and wait until the repository state is `Active`.

## Install the controller

1. In the `local` cluster, open **Apps → Charts**.
2. Filter by the `t-cloud-network-controller` repository.
3. Select **T-Cloud Network Controller** and then **Install**.
4. Set the release name to `t-cloud-network-controller`.
5. Set the namespace to `cattle-tcloud-system` and allow Rancher to create it.
6. Keep these values unless a private or mirrored registry is required:

   ```yaml
   image:
     repository: ghcr.io/opentelekomcloud/t-cloud-public-rancher-network-controller
     tag: ""
     pullPolicy: IfNotPresent
   leaderElection: true
   ```

   An empty image tag uses the chart's `appVersion`, which the release workflow
   sets to the release version.

7. Select **Install** and wait until the App state is `Deployed`.

## Verify the installation

Under **Apps → Installed Apps**, open `t-cloud-network-controller`. Its
Deployment should report one available replica. You can also open Rancher's
Kubectl Shell and run:

```bash
kubectl -n cattle-tcloud-system get deployment,pods
kubectl get crd tcloudclusternetworks.infrastructure.otc.t-systems.com
```

Refresh the browser after installation. The T-Cloud node-driver extension will
then detect the CRD and enable **Managed** shared networking.

## Upgrade

Publish a newer semantic release, refresh the repository under
**Apps → Repositories**, then open **Apps → Installed Apps**, select the
controller, and choose **Upgrade**. Review the version and values before
confirming.

## Safe uninstall

Do not uninstall the controller while managed networks exist. First delete the
associated Rancher clusters and confirm that this command returns no objects:

```bash
kubectl get tcloudclusternetworks.infrastructure.otc.t-systems.com -A
```

Then remove the App from **Apps → Installed Apps**. Removing the controller or
its CRD first can prevent shared VPC, subnet, and security-group cleanup.
