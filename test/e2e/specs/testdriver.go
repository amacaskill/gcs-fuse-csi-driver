/*
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
*/

package specs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/cloud_provider/clientset"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/cloud_provider/metadata"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/cloud_provider/storage"
	driver "github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/csi_driver"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/util"
	"github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	e2eframework "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
)

type GCSFuseCSITestDriver struct {
	driverInfo                  storageframework.DriverInfo
	clientset                   clientset.Interface
	meta                        metadata.Service
	storageServiceManager       storage.ServiceManager
	volumeStore                 []*gcsVolume
	bucketLocation              string
	ClientProtocol              string
	skipGcpSaTest               bool
	EnableHierarchicalNamespace bool
	EnableZB                    bool             // Enable Zonal Buckets
	storageService              *storage.Service // Client to interact with GCS
}

type gcsVolume struct {
	bucketName              string
	serviceAccountNamespace string
	mountOptions            string
	fileCacheCapacity       string
	shared                  bool
	readOnly                bool
	skipBucketAccessCheck   bool
	metadataPrefetch        bool
	enableMetrics           bool
}

// InitGCSFuseCSITestDriver returns GCSFuseCSITestDriver that implements TestDriver interface.
func InitGCSFuseCSITestDriver(c clientset.Interface, m metadata.Service, bl string, skipGcpSaTest, enableHierarchicalNamespace bool, clientProtocol string, enableZB bool) storageframework.TestDriver {
	ssm, err := storage.NewGCSServiceManager()
	if err != nil {
		e2eframework.Failf("Failed to set up storage service manager: %v", err)
	}

	return &GCSFuseCSITestDriver{
		driverInfo: storageframework.DriverInfo{
			Name:        driver.DefaultName,
			MaxFileSize: storageframework.FileSizeLarge,
			SupportedFsType: sets.NewString(
				"", // Default fsType
			),
			Capabilities: map[storageframework.Capability]bool{
				storageframework.CapPersistence: true,
				storageframework.CapExec:        true,
			},
		},
		clientset:                   c,
		meta:                        m,
		storageServiceManager:       ssm,
		volumeStore:                 []*gcsVolume{},
		bucketLocation:              bl,
		skipGcpSaTest:               skipGcpSaTest,
		ClientProtocol:              clientProtocol,
		EnableHierarchicalNamespace: enableHierarchicalNamespace,
		EnableZB:                    enableZB, // Enable Zonal Buckets
	}
}

var (
	_ storageframework.TestDriver                     = &GCSFuseCSITestDriver{}
	_ storageframework.PreprovisionedVolumeTestDriver = &GCSFuseCSITestDriver{}
	_ storageframework.PreprovisionedPVTestDriver     = &GCSFuseCSITestDriver{}
	_ storageframework.EphemeralTestDriver            = &GCSFuseCSITestDriver{}
	_ storageframework.DynamicPVTestDriver            = &GCSFuseCSITestDriver{}
)

func (n *GCSFuseCSITestDriver) GetDriverInfo() *storageframework.DriverInfo {
	return &n.driverInfo
}

func (n *GCSFuseCSITestDriver) SkipUnsupportedTest(pattern storageframework.TestPattern) {
	if pattern.VolType == storageframework.InlineVolume || pattern.VolType == storageframework.GenericEphemeralVolume {
		e2eskipper.Skipf("GCS CSI Fuse CSI Driver does not support %s -- skipping", pattern.VolType)
	}
}

func (n *GCSFuseCSITestDriver) PrepareTest(ctx context.Context, f *e2eframework.Framework) *storageframework.PerTestConfig {
	testK8sSA := NewTestKubernetesServiceAccount(f.ClientSet, f.Namespace, K8sServiceAccountName, "")
	var testGcpSA *TestGCPServiceAccount
	if !n.skipGcpSaTest {
		testGcpSA = NewTestGCPServiceAccount(prepareGcpSAName(f.Namespace.Name), n.meta.GetProjectID())
		testGcpSA.Create(ctx)
		testGcpSA.AddIAMPolicyBinding(ctx, f.Namespace)

		testK8sSA = NewTestKubernetesServiceAccount(f.ClientSet, f.Namespace, K8sServiceAccountName, testGcpSA.GetEmail())
	}
	testK8sSA.Create(ctx)

	config := &storageframework.PerTestConfig{
		Driver:    n,
		Framework: f,
	}

	ginkgo.DeferCleanup(func() {
		for _, v := range n.volumeStore {
			if err := n.deleteBucket(ctx, v.bucketName); err != nil {
				e2eframework.Logf("failed to delete bucket: %v", err)
			}
		}
		n.volumeStore = []*gcsVolume{}

		testK8sSA.Cleanup(ctx)
		if !n.skipGcpSaTest {
			testGcpSA.Cleanup(ctx)
		}
	})

	return config
}

func (n *GCSFuseCSITestDriver) CreateVolume(ctx context.Context, config *storageframework.PerTestConfig, volType storageframework.TestVolType) storageframework.TestVolume {
	switch volType {
	case storageframework.PreprovisionedPV:
		var bucketName string
		isMultipleBucketsPrefix := false

		switch config.Prefix {
		case FakeVolumePrefix, SkipCSIBucketAccessCheckAndFakeVolumePrefix, EnableMetadataPrefetchAndFakeVolumePrefix:
			bucketName = uuid.NewString()
		case InvalidVolumePrefix, SkipCSIBucketAccessCheckAndInvalidVolumePrefix:
			bucketName = InvalidVolume
		case ForceNewBucketPrefix, EnableFileCacheForceNewBucketPrefix, EnableMetadataPrefetchPrefixForceNewBucketPrefix, EnableFileCacheForceNewBucketAndMetricsPrefix:
			bucketName = n.createBucket(ctx, config.Framework.Namespace.Name)
		case MultipleBucketsPrefix:
			isMultipleBucketsPrefix = true
			l := []string{}
			for range 2 {
				bucketName = n.createBucket(ctx, config.Framework.Namespace.Name)
				n.volumeStore = append(n.volumeStore, &gcsVolume{
					bucketName:              bucketName,
					serviceAccountNamespace: config.Framework.Namespace.Name,
				})

				l = append(l, bucketName)
			}

			bucketName = "_"

			// Use config.Prefix to pass the bucket names back to the test suite.
			config.Prefix = strings.Join(l, ",")
		case SubfolderInBucketPrefix:
			if len(n.volumeStore) == 0 {
				bucketName = n.createBucket(ctx, config.Framework.Namespace.Name)
			} else {
				bucketName = n.volumeStore[len(n.volumeStore)-1].bucketName
			}
		default:
			if len(n.volumeStore) == 0 {
				bucketName = n.createBucket(ctx, config.Framework.Namespace.Name)
			} else {
				config.Prefix = n.volumeStore[0].bucketName

				return n.volumeStore[0]
			}
		}

		v := &gcsVolume{
			bucketName:              bucketName,
			serviceAccountNamespace: config.Framework.Namespace.Name,
		}
		mountOptions := "logging:severity:info"

		if n.ClientProtocol == "grpc" {
			mountOptions += ",client-protocol=grpc"
		}

		switch config.Prefix {
		case NonRootVolumePrefix:
			mountOptions += ",uid=1001"
		case InvalidMountOptionsVolumePrefix:
			mountOptions += ",invalid-option"
		case ImplicitDirsVolumePrefix:
			n.CreateImplicitDirInBucket(ctx, ImplicitDirsPath, bucketName)
			mountOptions += ",implicit-dirs"
		case SubfolderInBucketPrefix:
			dirPath := uuid.NewString()
			n.CreateImplicitDirInBucket(ctx, dirPath, bucketName)
			mountOptions += ",only-dir=" + dirPath
		case EnableFileCachePrefix, EnableFileCacheForceNewBucketPrefix:
			v.fileCacheCapacity = "100Mi"
		case EnableFileCacheAndMetricsPrefix, EnableFileCacheForceNewBucketAndMetricsPrefix:
			v.fileCacheCapacity = "100Mi"
			v.enableMetrics = true
		case EnableFileCacheWithLargeCapacityPrefix:
			v.fileCacheCapacity = "2Gi"
		case SkipCSIBucketAccessCheckPrefix, SkipCSIBucketAccessCheckAndFakeVolumePrefix, SkipCSIBucketAccessCheckAndInvalidVolumePrefix:
			v.skipBucketAccessCheck = true
		case SkipCSIBucketAccessCheckAndInvalidMountOptionsVolumePrefix:
			mountOptions += ",invalid-option"
			v.skipBucketAccessCheck = true
		case SkipCSIBucketAccessCheckAndNonRootVolumePrefix:
			mountOptions += ",uid=1001"
			v.skipBucketAccessCheck = true
		case SkipCSIBucketAccessCheckAndImplicitDirsVolumePrefix:
			n.CreateImplicitDirInBucket(ctx, ImplicitDirsPath, bucketName)
			mountOptions += ",implicit-dirs"
			v.skipBucketAccessCheck = true
		case EnableMetadataPrefetchPrefix, EnableMetadataPrefetchAndFakeVolumePrefix:
			mountOptions += ",file-system:kernel-list-cache-ttl-secs:-1"
			v.metadataPrefetch = true
		case EnableCustomReadAhead:
			mountOptions += ",read_ahead_kb=" + ReadAheadCustomReadAheadKb
		case EnableMetadataPrefetchAndInvalidMountOptionsVolumePrefix:
			mountOptions += ",file-system:kernel-list-cache-ttl-secs:-1,invalid-option"
			v.metadataPrefetch = true
		case DisableAutoconfig:
			mountOptions += ",disable-autoconfig"
		}

		v.mountOptions = mountOptions

		if !isMultipleBucketsPrefix {
			n.volumeStore = append(n.volumeStore, v)
		}

		switch config.Prefix {
		case "", EnableFileCachePrefix, EnableFileCacheWithLargeCapacityPrefix, EnableFileCacheAndMetricsPrefix:
			// Use config.Prefix to pass the bucket names back to the test suite.
			config.Prefix = bucketName
		}

		return v
	case storageframework.DynamicPV:
		// Do nothing
	default:
		e2eframework.Failf("Unsupported volType:%v is specified", volType)
	}

	return nil
}

func (v *gcsVolume) DeleteVolume(_ context.Context) {
	// Does nothing because the driver cleanup will delete all the buckets.
}

func (n *GCSFuseCSITestDriver) GetPersistentVolumeSource(readOnly bool, _ string, volume storageframework.TestVolume) (*corev1.PersistentVolumeSource, *corev1.VolumeNodeAffinity) {
	gv, _ := volume.(*gcsVolume)
	va := map[string]string{
		driver.VolumeContextKeyMountOptions: gv.mountOptions,
	}

	if gv.metadataPrefetch {
		va["gcsfuseMetadataPrefetchOnMount"] = "true"
		va["metadataStatCacheCapacity"] = "-1"
		va["metadataTypeCacheCapacity"] = "-1"
		va["metadataCacheTtlSeconds"] = "-1"
	}

	if gv.fileCacheCapacity != "" {
		va[driver.VolumeContextKeyFileCacheCapacity] = gv.fileCacheCapacity
	}

	if gv.skipBucketAccessCheck {
		va[driver.VolumeContextKeySkipCSIBucketAccessCheck] = util.TrueStr
	}

	if gv.enableMetrics {
		va[driver.VolumeContextKeyDisableMetrics] = util.FalseStr
	}

	return &corev1.PersistentVolumeSource{
		CSI: &corev1.CSIPersistentVolumeSource{
			Driver:           n.driverInfo.Name,
			VolumeHandle:     gv.bucketName,
			VolumeAttributes: va,
			ReadOnly:         readOnly,
		},
	}, nil
}

func (n *GCSFuseCSITestDriver) GetVolume(config *storageframework.PerTestConfig, _ int) (map[string]string, bool, bool) {
	volume := n.CreateVolume(context.Background(), config, storageframework.PreprovisionedPV)
	gv, _ := volume.(*gcsVolume)

	va := map[string]string{
		driver.VolumeContextKeyBucketName:   gv.bucketName,
		driver.VolumeContextKeyMountOptions: gv.mountOptions,
	}

	if gv.fileCacheCapacity != "" {
		va[driver.VolumeContextKeyFileCacheCapacity] = gv.fileCacheCapacity
	}

	if gv.skipBucketAccessCheck {
		va[driver.VolumeContextKeySkipCSIBucketAccessCheck] = util.TrueStr
	}

	if gv.metadataPrefetch {
		va["gcsfuseMetadataPrefetchOnMount"] = "true"
		va["metadataStatCacheCapacity"] = "-1"
		va["metadataTypeCacheCapacity"] = "-1"
		va["metadataCacheTtlSeconds"] = "-1"
	}

	if gv.enableMetrics {
		va[driver.VolumeContextKeyDisableMetrics] = util.FalseStr
	}

	return va, gv.shared, gv.readOnly
}

func (n *GCSFuseCSITestDriver) GetCSIDriverName(_ *storageframework.PerTestConfig) string {
	return n.driverInfo.Name
}

func (n *GCSFuseCSITestDriver) GetDynamicProvisionStorageClass(ctx context.Context, config *storageframework.PerTestConfig, _ string) *storagev1.StorageClass {
	// Set up the GCP Project IAM Policy
	member := fmt.Sprintf("serviceAccount:%v.svc.id.goog[%v/%v]", n.meta.GetProjectID(), config.Framework.Namespace.Name, K8sServiceAccountName)
	if !n.skipGcpSaTest {
		member = fmt.Sprintf("serviceAccount:%v@%v.iam.gserviceaccount.com", prepareGcpSAName(config.Framework.Namespace.Name), n.meta.GetProjectID())
	}
	testGCPProjectIAMPolicyBinding := NewTestGCPProjectIAMPolicyBinding(n.meta.GetProjectID(), member, "roles/storage.admin", "")
	testGCPProjectIAMPolicyBinding.Create(ctx)

	testSecret := NewTestSecret(config.Framework.ClientSet, config.Framework.Namespace, K8sSecretName, map[string]string{
		"projectID":               n.meta.GetProjectID(),
		"serviceAccountName":      K8sServiceAccountName,
		"serviceAccountNamespace": config.Framework.Namespace.Name,
	})
	testSecret.Create(ctx)

	ginkgo.DeferCleanup(func() {
		testSecret.Cleanup(ctx)
		testGCPProjectIAMPolicyBinding.Cleanup(ctx)
	})

	parameters := map[string]string{
		"csi.storage.k8s.io/provisioner-secret-name":      K8sSecretName,
		"csi.storage.k8s.io/provisioner-secret-namespace": "${pvc.namespace}",
	}
	generateName := "gcsfuse-csi-dynamic-test-sc-"
	defaultBindingMode := storagev1.VolumeBindingWaitForFirstConsumer

	mountOptions := []string{"debug_gcs", "debug_fuse", "debug_fs"}
	switch config.Prefix {
	case NonRootVolumePrefix:
		mountOptions = append(mountOptions, "uid=1001")
	case InvalidMountOptionsVolumePrefix:
		mountOptions = append(mountOptions, "invalid-option")
	}

	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: generateName,
		},
		Provisioner:       n.driverInfo.Name,
		MountOptions:      mountOptions,
		Parameters:        parameters,
		VolumeBindingMode: &defaultBindingMode,
	}
}

// prepareStorageService prepares the GCS Storage Service using the default GCP credentials.
func (n *GCSFuseCSITestDriver) prepareStorageService(ctx context.Context) (storage.Service, error) {
	storageService, err := n.storageServiceManager.SetupServiceWithDefaultCredential(ctx, n.EnableZB)
	if err != nil {
		return nil, fmt.Errorf("storage service manager failed to setup service: %w", err)
	}

	return storageService, nil
}

// SetIAMPolicy sets IAM policy for the GCS bucket.
func (n *GCSFuseCSITestDriver) SetIAMPolicy(ctx context.Context, bucket *storage.ServiceBucket, serviceAccountNamespace, serviceAccountName string) {
	storageService, err := n.prepareStorageService(ctx)
	if err != nil {
		e2eframework.Failf("Failed to prepare storage service: %v", err)
	}

	member := fmt.Sprintf("serviceAccount:%v.svc.id.goog[%v/%v]", n.meta.GetProjectID(), serviceAccountNamespace, serviceAccountName)
	if !n.skipGcpSaTest {
		member = fmt.Sprintf("serviceAccount:%v@%v.iam.gserviceaccount.com", prepareGcpSAName(serviceAccountNamespace), n.meta.GetProjectID())
	}
	if err := storageService.SetIAMPolicy(ctx, bucket, member, "roles/storage.admin"); err != nil {
		e2eframework.Failf("Failed to set the IAM policy for the new GCS bucket: %v", err)
	}
}

// RemoveIAMPolicy removes IAM policy from the GCS bucket.
func (n *GCSFuseCSITestDriver) RemoveIAMPolicy(ctx context.Context, bucket *storage.ServiceBucket, serviceAccountNamespace, serviceAccountName string) {
	storageService, err := n.prepareStorageService(ctx)
	if err != nil {
		e2eframework.Failf("Failed to prepare storage service: %v", err)
	}

	member := fmt.Sprintf("serviceAccount:%v.svc.id.goog[%v/%v]", n.meta.GetProjectID(), serviceAccountNamespace, serviceAccountName)
	if !n.skipGcpSaTest {
		member = fmt.Sprintf("serviceAccount:%v@%v.iam.gserviceaccount.com", prepareGcpSAName(serviceAccountNamespace), n.meta.GetProjectID())
	}
	if err := storageService.RemoveIAMPolicy(ctx, bucket, member, "roles/storage.admin"); err != nil {
		e2eframework.Failf("Failed to remove the IAM policy from the GCS bucket: %v", err)
	}
}

// createBucket creates a GCS bucket.
func (n *GCSFuseCSITestDriver) createBucket(ctx context.Context, serviceAccountNamespace string) string {
	storageService, err := n.prepareStorageService(ctx)
	if err != nil {
		e2eframework.Failf("Failed to prepare storage service: %v", err)
	}
	// the GCS bucket name is always new and unique,
	// so there is no need to check if the bucket already exists
	newBucket := &storage.ServiceBucket{
		Project:                        n.meta.GetProjectID(),
		Name:                           uuid.NewString(),
		Location:                       n.bucketLocation,
		EnableUniformBucketLevelAccess: true,
		EnableHierarchicalNamespace:    n.EnableHierarchicalNamespace,
		EnableZB:                       n.EnableZB,
	}

	ginkgo.By(fmt.Sprintf("Creating bucket %q", newBucket.Name))
	bucket, err := storageService.CreateBucket(ctx, newBucket)
	if err != nil {
		e2eframework.Failf("Failed to create a new GCS bucket: %v", err)
	}

	n.SetIAMPolicy(ctx, bucket, serviceAccountNamespace, K8sServiceAccountName)

	return bucket.Name
}

// deleteBucket deletes the GCS bucket.
func (n *GCSFuseCSITestDriver) deleteBucket(ctx context.Context, bucketName string) error {
	if bucketName == InvalidVolume {
		return nil
	}

	storageService, err := n.prepareStorageService(ctx)
	if err != nil {
		return fmt.Errorf("failed to prepare storage service: %w", err)
	}

	ginkgo.By(fmt.Sprintf("Deleting bucket %q", bucketName))
	err = storageService.DeleteBucket(ctx, &storage.ServiceBucket{Name: bucketName})
	if err != nil {
		return fmt.Errorf("failed to delete the GCS bucket: %w", err)
	}

	return nil
}

func prepareGcpSAName(ns string) string {
	if len(ns) > 30 {
		return ns[:30]
	}

	return ns
}

func (n *GCSFuseCSITestDriver) CreateImplicitDirInBucket(ctx context.Context, dirPath, bucketName string) {
	storageService, err := n.prepareStorageService(ctx)
	if err != nil {
		e2eframework.Failf("failed to prepare storage service: %w", err)
		return
	}
	// Use bucketName as the name of a temp file since bucketName is unique.
	f, err := os.CreateTemp("", "empty-data-file-*")
	if err != nil {
		e2eframework.Failf("Failed to create an empty data file: %v", err)
	}
	fileName := f.Name()
	f.Close()
	defer func() {
		err = os.Remove(fileName)
		if err != nil {
			e2eframework.Failf("Failed to delete the empty data file: %v", err)
		}
	}()
	if err := storageService.UploadGCSObject(ctx, fileName, bucketName, fmt.Sprintf("%s/", dirPath)); err != nil {
		e2eframework.Failf("UploadGCSObject failed: %v", err)
	}
}
