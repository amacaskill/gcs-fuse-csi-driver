<!--
Copyright 2018 The Kubernetes Authors.
Copyright 2022 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->

# Cloud Storage FUSE CSI Driver Manual Installation

> WARNING: This documentation describes how to manually install the driver to your GKE clusters. The manual installation should only be used for test purposes. To get official support from Google, users should use GKE to automatically deploy and manage the CSI driver as an add-on feature. See the GKE documentation [Access Cloud Storage buckets with the Cloud Storage FUSE CSI driver](https://cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/cloud-storage-fuse-csi-driver).
> NOTE: The manual installation only works on GKE Standard clusters. To enable the CSI driver on GKE Autopilot clusters, see the GKE documentation [Access Cloud Storage buckets with the Cloud Storage FUSE CSI driver](https://cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/cloud-storage-fuse-csi-driver).

## Prerequisites

- Clone the repo by running the following command.

  ```bash
  git clone https://github.com/GoogleCloudPlatform/gcs-fuse-csi-driver.git
  cd gcs-fuse-csi-driver
  ```

- Install `jq` utility.

  ```bash
  sudo apt-get update
  sudo apt-get install jq
  ```

- Create a standard GKE cluster with [Workload Identity](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity) enabled. Autopilot clusters are not supported for manual installation.
- Run the following commands to create a n2-standard-4 GKE cluster with Workload Identity enabled. Note other machine types may experience out of memory failures when running e2e tests.

  ```bash
  CLUSTER_PROJECT_ID=<cluster-project-id>
  CLUSTER_NAME=<cluster-name>
  gcloud container clusters create ${CLUSTER_NAME} --workload-pool=${CLUSTER_PROJECT_ID}.svc.id.goog --machine-type=n2-standard-4
  ```

- For an existing cluster, run the following commands to enable Workload Identity. Make sure machine type has enough memory if running e2e tests. Consider using machine type n2-standard-4.

  ```bash
  CLUSTER_PROJECT_ID=<cluster-project-id>
  CLUSTER_NAME=<cluster-name>
  gcloud container clusters update ${CLUSTER_NAME} --workload-pool=${CLUSTER_PROJECT_ID}.svc.id.goog
  ```

- Run the following command to ensure the kubectl context is set up correctly.

  ```bash
  gcloud container clusters get-credentials ${CLUSTER_NAME}

  # check the current context
  kubectl config current-context
  ```

## Install

You have two options for installing the Cloud Storage FUSE CSI Driver. You can use [Cloud build](#cloud-build), or [Makefile commands](#makefile-commands). The installation may take a few minutes.

### Cloud Build

If you would like to build your own images, follow the [Cloud Storage FUSE CSI Driver Development Guide - Cloud Build](development.md#cloud-build) to build and push the images with Cloud Build. After your image is built and pushed to your registry, run the following command to install the driver. The driver will be installed under a new namespace `gcs-fuse-csi-driver`.  The following commands assume you have created your artifact registry according to the [development guide prerequisites](development.md#prerequisites).

#### Prerequisites

Run the following command to grant the Cloud Build service account the Kubernetes Engine Admin (`roles/container.admin`) role which is required for the cluster to create cluster-wide resources (ClusterRole, ClusterRoleBinding), which is an admin-level task. `roles/container.developer` is also required for Cloud Build to be able to install the driver on the cluster, but this is covered within the `roles/container.admin` role.

```bash
export PROJECT_ID=$(gcloud config get project)
export PROJECT_NUMBER=$(gcloud projects describe ${PROJECT_ID} --format="value(projectNumber)")
gcloud projects add-iam-policy-binding $PROJECT_ID \
--member="serviceAccount:$PROJECT_NUMBER@cloudbuild.gserviceaccount.com" \
    --role="roles/container.admin"
```
- For `The policy contains bindings with conditions, so specifying a condition is required when adding a binding. Please specify a condition.:` You can enter: `2`(None).

#### Installing with Cloud Build on GKE Clusters

For GKE clusters, the cloud build script discovers the GKE cluster `_IDENTITY_PROVIDER` and `_IDENTITY_POOL` automatically, and these cannot be customized for GKE clusters. Note, the `_CLUSTER_NAME` and `_CLUSTER_LOCATION` are required to set up the kubectl config for the cloud build env. If you set a custom `STAGINGVERSION` when you [built your custom image](development.md#cloud-build), you must set the same `STAGINGVERSION` here. If you used the default when you [built your custom image](development.md#cloud-build), you should leave `STAGING_VERSION` unset.


```bash
# Replace with your cluster name
export CLUSTER_NAME=<cluster-name>
# Replace with your cluster location. This can be a zone, or a region depending on if your cluster is zonal or regional.
export CLUSTER_LOCATION=<cluster-location>
export PROJECT_ID=$(gcloud config get project)
export REGION='us-central1'
export REGISTRY="$REGION-docker.pkg.dev/$PROJECT_ID/csi-dev"
gcloud builds submit . --config=cloudbuild-install.yaml \
  --substitutions=_REGISTRY=$REGISTRY,_CLUSTER_NAME=$CLUSTER_NAME,_CLUSTER_LOCATION=$CLUSTER_LOCATION
```

#### Installing with Cloud Build on self built K8s Clusters

For non-GKE clusters (installing the driver on a self built k8s cluster), the installation process requires you to provide connection credentials and Workload Identity information that cannot be discovered automatically. You must set `_SELF_MANAGED_K8S` to `true`, and provide a Secret Manager secret containing your kubeconfig file following the instructions in [Creating a KUBECONFIG_SECRET](#creating-a-kubeconfig_secret). Additionally, you must explicitly set the `_IDENTITY_PROVIDER` and `_IDENTITY_POOL` variables. Please note that custom overrides of `_IDENTITY_PROVIDER` and `_IDENTITY_POOL` , is not supported for pods with host network yet. If you set a custom `STAGINGVERSION` when you [built your custom image](development.md#cloud-build), you must set the same `STAGINGVERSION` here. If you used the default when you [built your custom image](development.md#cloud-build), you should leave `STAGING_VERSION` unset.

```bash
# Replace with your cluster name
export CLUSTER_NAME=<cluster-name>
# Replace with your cluster location. This can be a zone, or a region depending on if your cluster is zonal or regional.
export CLUSTER_LOCATION=<cluster-location>
export PROJECT_ID=$(gcloud config get project)
export REGION='us-central1'
export REGISTRY="$REGION-docker.pkg.dev/$PROJECT_ID/csi-dev"
export IDENTITY_POOL="$PROJECT_ID.svc.id.goog"
export IDENTITY_PROVIDER="https://container.googleapis.com/v1/projects/$PROJECT_ID/locations/$CLUSTER_LOCATION/clusters/$CLUSTER_NAME"
 # The name of the secret you created in the "Creating a KUBECONFIG_SECRET section" below.
export KUBECONFIG_SECRET="gcsfuse-kubeconfig-secret"
gcloud builds submit . --config=cloudbuild-install.yaml \
  --substitutions=_REGISTRY=$REGISTRY,_PROJECT_ID=$PROJECT_ID,_IDENTITY_POOL=$IDENTITY_POOL,_IDENTITY_PROVIDER=$IDENTITY_PROVIDER,_CLUSTER_NAME=$CLUSTER_NAME,_CLUSTER_LOCATION=$CLUSTER_LOCATION,_SELF_MANAGED_K8S=true,_KUBECONFIG_SECRET=$KUBECONFIG_SECRET
```

##### Creating a KUBECONFIG_SECRET

Before running the build, you must create a secret in Google Secret Manager to securely store your `kubeconfig` file.

1. Grant Permissions to Cloud Build: The Cloud Build service account needs permission to access secrets in your project.

```bash
export PROJECT_ID=$(gcloud config get project)
export PROJECT_NUMBER=$(gcloud projects describe ${PROJECT_ID} --format="value(projectNumber)")
gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:$PROJECT_NUMBER@cloudbuild.gserviceaccount.com" \
    --role="roles/secretmanager.secretAccessor"
```
- For `The policy contains bindings with conditions, so specifying a condition is required when adding a binding. Please specify a condition.:` You can enter: `2`(None).

2. Create the Secret Container: This command creates an empty secret to hold your `kubeconfig`.

```bash
gcloud secrets create gcsfuse-kubeconfig-secret --replication-policy="automatic"
```

3. Upload Your `kubeconfig` as a New Version: This command reads your local `kubeconfig` file and uploads its contents to the secret you just created. Note, this uses the default `kubeconfig` location. If you are using a different location, replace `~/.kube/config` with the location of your `kubeconfig` file.

```bash
cat ~/.kube/config | gcloud secrets versions add gcsfuse-kubeconfig-secret --data-file=-
```

##### Troubleshooting: Kubeconfig File is Too Large

When following [Creating a KUBECONFIG_SECRET](#creating-a-kubeconfig_secret), if you see an error message like `The file [-] is larger than the maximum size of 65536 bytes`, it means your `kubeconfig` file contains connection details for multiple clusters and has exceeded Secret Manager's `64KiB` size limit.

To fix this, generate a minimal, self-contained `kubeconfig` file that only includes the details for your target cluster.

1. Set Your `kubectl` Context: Ensure `kubectl` is pointing to the correct self-managed cluster.

```bash
export CLUSTER_NAME=<cluster-name>
kubectl config use-context $CLUSTER_NAME
```

2. Generate a Minimal `kubeconfig`: This command creates a new, clean file with only the necessary information for the current context. The `--flatten` flag embeds credentials directly into the file.

```bash
kubectl config view --flatten --minify > minimal-kubeconfig.yaml
```

3. Upload the Minimal `kubeconfig`: Use this new, smaller file to add the secret version.

```bash
cat minimal-kubeconfig.yaml | gcloud secrets versions add gcsfuse-kubeconfig-secret --data-file=-
```

### Makefile Commands

- Run the following command to install the latest driver version. Note, the default registry is a GOOGLE internal project, so only Google Internal employees can currently run this. The driver will be installed under a new namespace `gcs-fuse-csi-driver`.

  ```bash
  latest_version=$(git tag --sort=-creatordate | head -n 1)
  # Replace <cluster-project-id> with your cluster project ID.
  make install STAGINGVERSION=$latest_version PROJECT=<cluster-project-id>
  ```

- If you would like to build your own images, follow the [Cloud Storage FUSE CSI Driver Development Guide](development.md) to build and push the images. Run the following command to install the driver.

  ```bash
  # Specify the image registry and image version if you have built the images from source code.
  make install REGISTRY=<your-container-registry> STAGINGVERSION=<staging-version> PROJECT=<cluster-project-id>
  ```

By default, the `Makefile` discovers the GKE cluster `IDENTITY_PROVIDER` and `IDENTITY_POOL` automatically. To override them, pass the variables with the `make` command (note: this custom override does not work for pods with host network yet). For example:
```bash
make install REGISTRY=<your-container-registry> STAGINGVERSION=<staging-version> IDENTITY_PROVIDER=<your-identity-provider> IDENTITY_POOL=<your-identity-pool>
```

By default, the CSI driver performs a Workload Identity node label check during NodePublishVolume to ensure the GKE metadata server is available on the node. To disable this check, set the `WI_NODE_LABEL_CHECK` environment variable to `false`:

## Check the Driver Status

The output from the following command

```bash
kubectl get CSIDriver,Deployment,DaemonSet,Pods -n gcs-fuse-csi-driver
```

should contain the driver application information, something like

```text
NAME                                                  ATTACHREQUIRED   PODINFOONMOUNT   STORAGECAPACITY   TOKENREQUESTS                              REQUIRESREPUBLISH   MODES                  AGE
csidriver.storage.k8s.io/gcsfuse.csi.storage.gke.io   false            true             false             <cluster-project-id>-gke-dev.svc.id.goog   true                Persistent,Ephemeral   3m49s

NAME                                          READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/gcs-fuse-csi-driver-webhook   1/1     1            1           3m49s

NAME                               DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR            AGE
daemonset.apps/gcsfusecsi-node     3         3         3       3            3           kubernetes.io/os=linux   3m49s

NAME                                               READY   STATUS    RESTARTS   AGE
pod/gcs-fuse-csi-driver-webhook-565f85dcb9-pdlb9   1/1     Running   0          3m49s
pod/gcsfusecsi-node-b6rs2                          2/2     Running   0          3m49s
pod/gcsfusecsi-node-ng9xs                          2/2     Running   0          3m49s
pod/gcsfusecsi-node-t9zq5                          2/2     Running   0          3m49s
```

## Uninstall

You have two options for un-installing the Cloud Storage FUSE CSI Driver. You can use cloud build, or run the makefile commands. 

### Cloud Build

TODO(amacaskill): Add cloud build for uninstall.


### Makefile Commands
- Run the following command to uninstall the driver.

  ```bash
  make uninstall
  ```