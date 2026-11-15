/*
Portions Copyright (c) Microsoft Corporation.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package status_test

import (
	"context"
	stderrors "errors"
	"slices"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclass/status"
	"github.com/Azure/karpenter-provider-azure/pkg/fake"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/test"

	"github.com/samber/lo"

	opstatus "github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

type regionalTestImages struct{ images []imagefamily.NodeImage }

func (p *regionalTestImages) List(context.Context, *v1beta1.AKSNodeClass) ([]imagefamily.NodeImage, error) {
	return p.images, nil
}

var _ = Describe("Multi-region image maintenance", func() {
	It("should retain each region's version of the same image definition outside maintenance", func() {
		nc := test.AKSNodeClass()
		nc.StatusConditions().SetTrue(v1beta1.ConditionTypeImagesReady)
		nc.Status.Images = []v1beta1.NodeImage{
			{ID: "gallery/image/versions/1", Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelTopologyRegion, Operator: corev1.NodeSelectorOpIn, Values: []string{"eastus2"}}}},
			{ID: "gallery/image/versions/2", Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelTopologyRegion, Operator: corev1.NodeSelectorOpIn, Values: []string{"westus2"}}}},
		}
		expected := nc.DeepCopy().Status.Images
		provider := &regionalTestImages{images: []imagefamily.NodeImage{
			{ID: "gallery/image/versions/3", Requirements: scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "eastus2"))},
			{ID: "gallery/image/versions/4", Requirements: scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "westus2"))},
		}}
		kube := kubernetesfake.NewClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "upcoming-maintenance-window", Namespace: "kube-system"},
			Data: map[string]string{
				"aksManagedNodeOSUpgradeSchedule-start": time.Now().Add(time.Hour).Format(time.RFC3339),
				"aksManagedNodeOSUpgradeSchedule-end":   time.Now().Add(2 * time.Hour).Format(time.RFC3339),
			},
		})
		_, err := status.NewNodeImageReconciler(provider, kube, "kube-system").Reconcile(ctx, nc)
		Expect(err).ToNot(HaveOccurred())
		Expect(nc.Status.Images).To(Equal(expected))
	})
})

const (
	oldcigImageVersion   = "202410.09.0"
	newCIGImageVersion   = "202501.02.0"
	oldSIGImageVersion   = "202410.09.0"
	newSIGImageVersion   = "202608.26.0"
	rollbackImageVersion = "202409.03.0"
)

func getExpectedTestCommunityImages(
	k8sVersion string,
	apiVersion string,
) []v1beta1.NodeImage {
	//TODO: This is too close to replicating the logic in the actual controller;
	// We should instead assert on properties of the resulting NodeImage objects rather than replicating the controller logic.
	var kubernetesVersion *version.Version
	if k8sVersion != "" {
		kubernetesVersion = version.MustParseSemantic(k8sVersion)
	}

	galleryURL := "AKSUbuntu-38d80f77-467a-481f-a8d4-09b6d4220bd2"
	gen1 := imagefamily.Ubuntu2204Gen1ImageDefinition
	gen2 := imagefamily.Ubuntu2204Gen2ImageDefinition
	arm := imagefamily.Ubuntu2204Gen2ArmImageDefinition

	// From Kubernetes version 1.34, default to Ubuntu 2404
	v134 := version.MustParse("1.34.0")
	if kubernetesVersion != nil && kubernetesVersion.AtLeast(v134) {
		// Logic for Ubuntu 2404 images can be added here
		gen1 = imagefamily.Ubuntu2404Gen1ImageDefinition
		gen2 = imagefamily.Ubuntu2404Gen2ImageDefinition
		arm = imagefamily.Ubuntu2404Gen2ArmImageDefinition
	}

	return []v1beta1.NodeImage{
		{
			ID: imagefamily.BuildImageIDCIG(galleryURL, gen2, apiVersion),
			Requirements: []corev1.NodeSelectorRequirement{
				{
					Key:      corev1.LabelArchStable,
					Operator: "In",
					Values:   []string{"amd64"},
				},
				{
					Key:      v1beta1.LabelSKUHyperVGeneration,
					Operator: "In",
					Values:   []string{"2"},
				},
			},
		},
		{
			ID: imagefamily.BuildImageIDCIG(galleryURL, gen1, apiVersion),
			Requirements: []corev1.NodeSelectorRequirement{
				{
					Key:      corev1.LabelArchStable,
					Operator: "In",
					Values:   []string{"amd64"},
				},
				{
					Key:      v1beta1.LabelSKUHyperVGeneration,
					Operator: "In",
					Values:   []string{"1"},
				},
			},
		},
		{
			ID: imagefamily.BuildImageIDCIG(galleryURL, arm, apiVersion),
			Requirements: []corev1.NodeSelectorRequirement{
				{
					Key:      corev1.LabelArchStable,
					Operator: "In",
					Values:   []string{"arm64"},
				},
				{
					Key:      v1beta1.LabelSKUHyperVGeneration,
					Operator: "In",
					Values:   []string{"2"},
				},
			},
		},
	}
}

func getExpectedTestSIGImages(version string) []v1beta1.NodeImage {
	nodeClass := test.AKSNodeClass(test.WithVersionBasedDefaults(env.Version))
	nodeClass.Status.KubernetesVersion = lo.ToPtr(testK8sVersion)
	test.ApplySIGImagesWithVersion(nodeClass, version)
	return nodeClass.Status.Images
}

func getClosedMWConfigMap() *corev1.ConfigMap {
	configMap := getEmptyMWConfigMap()
	startTime := time.Now().Add(time.Hour).UTC()
	endTime := time.Now().Add(2 * time.Hour).UTC()
	configMap.Data["aksManagedNodeOSUpgradeSchedule-start"] = startTime.Format(time.RFC3339)
	configMap.Data["aksManagedNodeOSUpgradeSchedule-end"] = endTime.Format(time.RFC3339)
	return configMap
}

func getOpenMWConfigMap() *corev1.ConfigMap {
	configMap := getEmptyMWConfigMap()
	startTime := time.Now().Add(-time.Hour).UTC()
	endTime := time.Now().Add(time.Hour).UTC()
	configMap.Data["aksManagedNodeOSUpgradeSchedule-start"] = startTime.Format(time.RFC3339)
	configMap.Data["aksManagedNodeOSUpgradeSchedule-end"] = endTime.Format(time.RFC3339)
	return configMap
}

func getEmptyMWConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upcoming-maintenance-window",
			Namespace: "kube-system",
		},
		Data: map[string]string{},
	}
}

// getEmptyValuesMWConfigMap returns a ConfigMap with all maintenance window keys present
// but with empty string values, which may be observed under some circumstances
func getEmptyValuesMWConfigMap() *corev1.ConfigMap {
	configMap := getEmptyMWConfigMap()
	configMap.Data["aksManagedAutoUpgradeSchedule-start"] = ""
	configMap.Data["aksManagedAutoUpgradeSchedule-end"] = ""
	configMap.Data["aksManagedNodeOSUpgradeSchedule-start"] = ""
	configMap.Data["aksManagedNodeOSUpgradeSchedule-end"] = ""
	configMap.Data["default-start"] = ""
	configMap.Data["default-end"] = ""
	return configMap
}

var _ = Describe("NodeClass NodeImage Status Controller", func() {
	var nodeClass *v1beta1.AKSNodeClass

	BeforeEach(func() {
		nodeClass = test.AKSNodeClass()
	})

	testCases := []struct {
		name           string
		useSIG         bool
		oldVersion     string
		latestVersion  string
		expectedImages func(string) []v1beta1.NodeImage
	}{
		{
			name:          "CIG",
			oldVersion:    oldcigImageVersion,
			latestVersion: newCIGImageVersion,
			expectedImages: func(api string) []v1beta1.NodeImage {
				return getExpectedTestCommunityImages(env.Version.String(), api)
			},
		},
		{
			name:           "SIG",
			useSIG:         true,
			oldVersion:     oldSIGImageVersion,
			latestVersion:  newSIGImageVersion,
			expectedImages: getExpectedTestSIGImages,
		},
	}

	for _, testCase := range testCases {

		Context("with "+testCase.name, func() {
			BeforeEach(func() {
				if testCase.useSIG {
					ctx = test.Options(test.OptionsFields{
						UseSIG:            lo.ToPtr(true),
						SIGSubscriptionID: lo.ToPtr("10945678-1234-1234-1234-123456789012"),
					}).ToContext(ctx)
					return
				}

				cigImageVersionTest := testCase.latestVersion
				azureEnv.CommunityImageVersionsAPI.ImageVersions.Append(&armcompute.CommunityGalleryImageVersion{Name: &cigImageVersionTest})
			})

			It("should init Images and its readiness on AKSNodeClass", func() {
				ExpectApplied(ctx, env.Client, nodeClass)
				ExpectObjectReconciled(ctx, env.Client, controller, nodeClass)
				nodeClass = ExpectExists(ctx, env.Client, nodeClass)

				ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
			})

			It("should update Images and its readiness on AKSNodeClass", func() {
				nodeClass.Status.Images = testCase.expectedImages(testCase.oldVersion)
				nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeImagesReady)

				ExpectApplied(ctx, env.Client, nodeClass)
				nodeClass = ExpectExists(ctx, env.Client, nodeClass)

				Expect(nodeClass.Status.Images).To(HaveExactElements(testCase.expectedImages(testCase.oldVersion)))
				Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())

				ExpectObjectReconciled(ctx, env.Client, controller, nodeClass)
				nodeClass = ExpectExists(ctx, env.Client, nodeClass)

				Expect(len(nodeClass.Status.Images)).To(Equal(3))
				Expect(nodeClass.Status.Images).To(HaveExactElements(testCase.expectedImages(testCase.latestVersion)))
				Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
			})

			Context("NodeImageReconciler direct tests", func() {
				BeforeEach(func() {
					// Setup NodeClass
					nodeClass.Status.KubernetesVersion = lo.ToPtr(testK8sVersion)
					nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeKubernetesVersionReady)

					nodeClass.Status.Images = testCase.expectedImages(testCase.oldVersion)
					nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeImagesReady)
				})

				When("versions are requested", func() {
					var imageReconciler *status.NodeImageReconciler

					BeforeEach(func() {
						imageReconciler = status.NewNodeImageReconciler(azureEnv.ImageProvider, env.KubernetesInterface, "kube-system")
						ExpectApplied(ctx, env.Client, getClosedMWConfigMap())

						nodeClass.Status.ObservedVersions = &v1beta1.ObservedVersions{
							CurrentControlPlaneKubernetesVersion: lo.ToPtr(testK8sVersion),
							LatestImageVersion:                   lo.ToPtr(testCase.latestVersion),
						}
					})

					It("should bypass the maintenance window when images are not ready", func() {
						nodeClass.StatusConditions().SetFalse(v1beta1.ConditionTypeImagesReady, "KubernetesVersionChanged", "Kubernetes version changed")
						nodeClass.Spec.Versions = &v1beta1.Versions{
							KubernetesVersion: lo.ToPtr(testK8sVersion),
						}

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
					})

					It("should recover after an invalid image version is corrected", func() {
						nodeClass.StatusConditions().SetFalse(v1beta1.ConditionTypeImagesReady, "NodeImageVersionInvalid", "invalid image version")
						nodeClass.Spec.Versions = &v1beta1.Versions{
							KubernetesVersion: lo.ToPtr(testK8sVersion),
							NodeImageVersion:  lo.ToPtr(testCase.latestVersion),
						}

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
					})

					It("should replace image suffixes with the requested version outside the maintenance window", func() {
						nodeClass.Spec.Versions = &v1beta1.Versions{
							KubernetesVersion: lo.ToPtr(testK8sVersion),
							NodeImageVersion:  lo.ToPtr(testCase.oldVersion),
						}

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.oldVersion, testCase.expectedImages)
					})

					It("should immediately return to the latest image when the pin is removed", func() {
						nodeClass.Spec.Versions = &v1beta1.Versions{
							KubernetesVersion: lo.ToPtr(testK8sVersion),
							NodeImageVersion:  lo.ToPtr(testCase.oldVersion),
						}

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())
						Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).Reason).To(Equal("NodeImageVersionPinned"))

						nodeClass.Spec.Versions.NodeImageVersion = nil
						_, err = imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
						Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).Reason).NotTo(Equal("NodeImageVersionPinned"))
					})

					It("should reject an image version that was not found in status", func() {
						nodeClass.Spec.Versions = &v1beta1.Versions{
							KubernetesVersion: lo.ToPtr(testK8sVersion),
							NodeImageVersion:  lo.ToPtr("not-found"),
						}

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						condition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady)
						Expect(condition.IsFalse()).To(BeTrue())
						Expect(condition.Reason).To(Equal("NodeImageVersionInvalid"))
						Expect(nodeClass.Status.Images).To(HaveExactElements(testCase.expectedImages(testCase.oldVersion)))
					})

					It("should reject a malformed image version", func() {
						malformedImageVersion := "invalid/version"
						nodeClass.Status.ObservedVersions.LatestImageVersion = &malformedImageVersion
						nodeClass.Spec.Versions = &v1beta1.Versions{
							KubernetesVersion: lo.ToPtr(testK8sVersion),
							NodeImageVersion:  lo.ToPtr(malformedImageVersion),
						}

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						condition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady)
						Expect(condition.IsFalse()).To(BeTrue())
						Expect(condition.Reason).To(Equal("RequestedNodeImageVersionUnavailable"))
						Expect(nodeClass.Status.Images).To(HaveExactElements(testCase.expectedImages(testCase.oldVersion)))
					})

					It("should roll back to a recently used image version outside the maintenance window", func() {
						nodeClass.Status.ObservedVersions.RecentlyUsedVersions = []v1beta1.RecentlyUsedVersion{
							{
								NodeImageVersion:  lo.ToPtr(rollbackImageVersion),
								KubernetesVersion: lo.ToPtr(testK8sVersion),
							},
						}
						nodeClass.Spec.Versions = &v1beta1.Versions{
							KubernetesVersion: lo.ToPtr(testK8sVersion),
							NodeImageVersion:  lo.ToPtr(rollbackImageVersion),
						}

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, rollbackImageVersion, testCase.expectedImages)
					})

					It("should reject a rollback image paired with a different Kubernetes version", func() {
						nodeClass.Status.ObservedVersions.RecentlyUsedVersions = []v1beta1.RecentlyUsedVersion{
							{
								NodeImageVersion:  lo.ToPtr(rollbackImageVersion),
								KubernetesVersion: lo.ToPtr(oldK8sVersion),
							},
						}
						nodeClass.Spec.Versions = &v1beta1.Versions{
							KubernetesVersion: lo.ToPtr(testK8sVersion),
							NodeImageVersion:  lo.ToPtr(rollbackImageVersion),
						}

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						condition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady)
						Expect(condition.IsFalse()).To(BeTrue())
						Expect(condition.Reason).To(Equal("RollbackTargetKubernetesVersionMismatch"))
						Expect(nodeClass.Status.Images).To(HaveExactElements(testCase.expectedImages(testCase.oldVersion)))
					})

				})

				When("SYSTEM_NAMESPACE is set", func() {
					var (
						imageReconciler *status.NodeImageReconciler
					)

					BeforeEach(func() {
						imageReconciler = status.NewNodeImageReconciler(azureEnv.ImageProvider, env.KubernetesInterface, "kube-system")
					})

					It("Should update NodeImages when ConfigMap is missing (fail open)", func() {
						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
					})

					It("Should not update NodeImages when maintenance window is not open", func() {
						ExpectApplied(ctx, env.Client, getClosedMWConfigMap())

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.oldVersion, testCase.expectedImages)
					})

					It("Should update NodeImages when ConfigMap is empty (maintenance window undefined)", func() {
						ExpectApplied(ctx, env.Client, getEmptyMWConfigMap())

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
					})

					It("Should update NodeImages when ConfigMap has keys with empty string values (fail open)", func() {
						ExpectApplied(ctx, env.Client, getEmptyValuesMWConfigMap())

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
					})

					It("Should update NodeImages when maintenance window is open", func() {
						ExpectApplied(ctx, env.Client, getOpenMWConfigMap())

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
					})

					It("Should error when ConfigMap is malformed (missing endtime)", func() {
						configMap := getOpenMWConfigMap()
						delete(configMap.Data, "aksManagedNodeOSUpgradeSchedule-end")
						ExpectApplied(ctx, env.Client, configMap)

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).To(HaveOccurred())
						Expect(err.Error()).To(ContainSubstring("unexpected state, with incomplete maintenance window data for channel aksManagedNodeOSUpgradeSchedule"))

						ExpectReadyWithImages(nodeClass, testCase.oldVersion, testCase.expectedImages)
					})

					It("Should error when ConfigMap is malformed (invalid timestamp)", func() {
						configMap := getOpenMWConfigMap()
						configMap.Data["aksManagedNodeOSUpgradeSchedule-end"] = "invalid-timestamp"
						ExpectApplied(ctx, env.Client, configMap)

						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).To(HaveOccurred())
						Expect(err.Error()).To(ContainSubstring("error parsing maintenance window end time for channel aksManagedNodeOSUpgradeSchedule"))

						ExpectReadyWithImages(nodeClass, testCase.oldVersion, testCase.expectedImages)
					})
				})

				When("the system namespace is not set", func() {
					var (
						imageReconciler *status.NodeImageReconciler
					)

					BeforeEach(func() {
						imageReconciler = status.NewNodeImageReconciler(azureEnv.ImageProvider, env.KubernetesInterface, "")
					})

					It("Should update NodeImages (fail open)", func() {
						_, err := imageReconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())

						ExpectReadyWithImages(nodeClass, testCase.latestVersion, testCase.expectedImages)
					})
				})
			})
		})
	}

	Context("FIPS Validation With UseSIG", func() {
		var imageReconciler *status.NodeImageReconciler

		BeforeEach(func() {
			ctx = test.Options(test.OptionsFields{
				UseSIG: lo.ToPtr(false),
			}).ToContext(ctx)

			nodeClass.Status.KubernetesVersion = lo.ToPtr(testK8sVersion)
			nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeKubernetesVersionReady)
			nodeClass.Status.Images = getExpectedTestCommunityImages(testK8sVersion, oldcigImageVersion)
			nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeImagesReady)
			imageReconciler = status.NewNodeImageReconciler(azureEnv.ImageProvider, env.KubernetesInterface, "")
		})

		It("images ready status should be false if FIPS is enabled but UseSIG is false", func() {
			nodeClass.Spec.FIPSMode = &v1beta1.FIPSModeFIPS
			imageFamily := v1beta1.AzureLinuxImageFamily
			nodeClass.Spec.ImageFamily = &imageFamily

			result, err := imageReconciler.Reconcile(ctx, nodeClass)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(nodeClass.Status.Images).To(BeNil())

			condition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady)
			Expect(condition.IsFalse()).To(BeTrue())
			Expect(condition.Reason).To(Equal("SIGRequiredForFIPS"))
			Expect(condition.Message).To(Equal("FIPS images require UseSIG to be enabled, but UseSIG is false (note: UseSIG is only supported in AKS managed NAP)"))

			readyCondition := nodeClass.StatusConditions().Get(opstatus.ConditionReady)
			Expect(readyCondition.IsFalse()).To(BeTrue())
		})

		Context("with --community-gallery-fips-images", func() {
			const (
				fipsVersion       = "202610.30.1"
				newerFIPSVersion  = "202611.06.0"
				newestFIPSVersion = "202611.13.0"
			)
			fipsDefinitions := func() []string {
				family := imagefamily.GetImageFamily(nodeClass.Spec.ImageFamily, nodeClass.Spec.FIPSMode, false, testK8sVersion, nil)
				defaultImages := family.DefaultImages(true, nodeClass.Spec.FIPSMode, false, false)
				names := make([]string, 0, len(defaultImages))
				for _, image := range defaultImages {
					names = append(names, image.ImageDefinition)
				}
				return names
			}
			fipsImageIDs := func(version string) []string {
				return lo.Map(fipsDefinitions(), func(name string, _ int) string {
					return imagefamily.BuildImageIDCIG(imagefamily.AKSAzureLinuxPublicGalleryURL, name, version)
				})
			}
			statusImageIDs := func() []string {
				return lo.Map(nodeClass.Status.Images, func(image v1beta1.NodeImage, _ int) string { return image.ID })
			}
			// closedWindowReconciler reconciles images while the node OS maintenance window is closed.
			closedWindowReconciler := func() *status.NodeImageReconciler {
				kube := kubernetesfake.NewClientset(&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "upcoming-maintenance-window", Namespace: "kube-system"},
					Data: map[string]string{
						"aksManagedNodeOSUpgradeSchedule-start": time.Now().Add(time.Hour).Format(time.RFC3339),
						"aksManagedNodeOSUpgradeSchedule-end":   time.Now().Add(2 * time.Hour).Format(time.RFC3339),
					},
				})
				return status.NewNodeImageReconciler(azureEnv.ImageProvider, kube, "kube-system")
			}
			// withoutFIPSImages makes the region list no FIPS image.
			withoutFIPSImages := func() {
				azureEnv.CommunityImageVersionsAPI.NotFoundImageDefinitions = fipsDefinitions()
				azureEnv.NodeImagesCache.Flush()
			}
			// withNewerFIPSImages makes the region list its FIPS images again, at a version published after fipsVersion.
			withNewerFIPSImages := func() {
				azureEnv.CommunityImageVersionsAPI.NotFoundImageDefinitions = nil
				azureEnv.CommunityImageVersionsAPI.ImageVersions.Append(&armcompute.CommunityGalleryImageVersion{
					Name:       lo.ToPtr(newerFIPSVersion),
					Properties: &armcompute.CommunityGalleryImageVersionProperties{PublishedDate: lo.ToPtr(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC))},
				})
				azureEnv.NodeImagesCache.Flush()
			}
			// withFIPSImagesExcept makes the region list the FIPS images of every image definition except unlisted.
			withFIPSImagesExcept := func(unlisted ...string) {
				azureEnv.CommunityImageVersionsAPI.NotFoundImageDefinitions = unlisted
				azureEnv.NodeImagesCache.Flush()
			}
			// publishFIPSImages publishes the FIPS images at the named version on the given day of November 2026.
			publishFIPSImages := func(name string, day int) {
				azureEnv.CommunityImageVersionsAPI.ImageVersions.Append(&armcompute.CommunityGalleryImageVersion{
					Name:       lo.ToPtr(name),
					Properties: &armcompute.CommunityGalleryImageVersionProperties{PublishedDate: lo.ToPtr(time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC))},
				})
				azureEnv.NodeImagesCache.Flush()
			}
			// unreadableWindow is a cluster in which the maintenance window ConfigMap cannot be read.
			unreadableWindow := func() kubernetes.Interface {
				kube := kubernetesfake.NewClientset()
				kube.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), "upcoming-maintenance-window", stderrors.New("access denied"))
				})
				return kube
			}
			// incompleteWindow is a cluster whose maintenance window ConfigMap has a start but no end.
			incompleteWindow := func() kubernetes.Interface {
				return kubernetesfake.NewClientset(&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "upcoming-maintenance-window", Namespace: "kube-system"},
					Data:       map[string]string{"aksManagedNodeOSUpgradeSchedule-start": time.Now().Add(time.Hour).Format(time.RFC3339)},
				})
			}
			// unparseableWindow is a cluster whose maintenance window ConfigMap has a start time that does not parse.
			unparseableWindow := func() kubernetes.Interface {
				return kubernetesfake.NewClientset(&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "upcoming-maintenance-window", Namespace: "kube-system"},
					Data: map[string]string{
						"aksManagedNodeOSUpgradeSchedule-start": "not-a-time",
						"aksManagedNodeOSUpgradeSchedule-end":   time.Now().Add(2 * time.Hour).Format(time.RFC3339),
					},
				})
			}

			BeforeEach(func() {
				ctx = test.Options(test.OptionsFields{
					UseSIG:                     lo.ToPtr(false),
					CommunityGalleryFIPSImages: lo.ToPtr(true),
				}).ToContext(ctx)
				nodeClass.Spec.FIPSMode = &v1beta1.FIPSModeFIPS
				nodeClass.Spec.ImageFamily = lo.ToPtr(v1beta1.AzureLinuxImageFamily)
				// A FIPS NodeClass that has not resolved images yet.
				nodeClass.Status.Images = nil
				nodeClass.StatusConditions().SetFalse(v1beta1.ConditionTypeImagesReady, "Unresolved", "not resolved yet")
				azureEnv.NodeImagesCache.Flush()
				azureEnv.CommunityImageVersionsAPI.ImageVersions.Append(&armcompute.CommunityGalleryImageVersion{
					Name:       lo.ToPtr(fipsVersion),
					Properties: &armcompute.CommunityGalleryImageVersionProperties{PublishedDate: lo.ToPtr(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))},
				})
				DeferCleanup(func() { azureEnv.CommunityImageVersionsAPI.NotFoundImageDefinitions = nil })
			})

			It("resolves only FIPS images from the community gallery", func() {
				_, err := imageReconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
				Expect(statusImageIDs()).To(ConsistOf(fipsImageIDs(fipsVersion)))
			})

			It("reports an image family without community gallery FIPS images", func() {
				nodeClass.Spec.ImageFamily = lo.ToPtr(v1beta1.UbuntuImageFamily)
				_, err := imageReconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				condition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady)
				Expect(condition.IsFalse()).To(BeTrue())
				Expect(condition.Reason).To(Equal(imagefamily.CommunityGalleryFIPSImagesUnavailableReason))
				Expect(nodeClass.Status.Images).To(BeNil())
			})

			It("names the region without a FIPS image and keeps the ready images", func() {
				azureEnv.CommunityImageVersionsAPI.NotFoundImageDefinitions = fipsDefinitions()
				resolved := []v1beta1.NodeImage{{ID: fipsImageIDs(fipsVersion)[0]}}
				nodeClass.Status.Images = resolved
				nodeClass.StatusConditions().SetTrue(v1beta1.ConditionTypeImagesReady)
				result, err := imageReconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).ToNot(BeZero())
				condition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady)
				Expect(condition.IsFalse()).To(BeTrue())
				Expect(condition.Reason).To(Equal(imagefamily.FIPSImagesNotFoundReason))
				Expect(condition.Message).To(ContainSubstring("region " + fake.Region))
				Expect(nodeClass.Status.Images).To(Equal(resolved))
			})

			It("keeps the image versions in use when the FIPS images return while the maintenance window is closed", func() {
				reconciler := closedWindowReconciler()
				_, err := reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(statusImageIDs()).To(ConsistOf(fipsImageIDs(fipsVersion)))
				inUse := nodeClass.DeepCopy().Status.Images

				withoutFIPSImages()
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).Reason).To(Equal(imagefamily.FIPSImagesNotFoundReason))
				Expect(nodeClass.Status.Images).To(Equal(inUse))

				withNewerFIPSImages()
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
				Expect(nodeClass.Status.Images).To(Equal(inUse))
			})

			It("completes a pending image update when the FIPS images return", func() {
				reconciler := closedWindowReconciler()
				_, err := reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())

				// A Kubernetes upgrade requests the newest images while the region lists no FIPS image.
				nodeClass.StatusConditions().SetFalse(v1beta1.ConditionTypeImagesReady, "KubernetesUpgrade", "Performing kubernetes upgrade, need to get latest images")
				withoutFIPSImages()
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).Reason).To(Equal(imagefamily.FIPSImagesNotFoundReason))
				Expect(nodeClass.Status.Images).To(BeEmpty())

				withNewerFIPSImages()
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
				Expect(statusImageIDs()).To(ConsistOf(fipsImageIDs(newerFIPSVersion)))

				// The update is complete, so the maintenance window governs the images again.
				withoutFIPSImages()
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				publishFIPSImages(newestFIPSVersion, 13)
				withFIPSImagesExcept()
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
				Expect(statusImageIDs()).To(ConsistOf(fipsImageIDs(newerFIPSVersion)))
			})

			It("keeps the images in use in their priority order while the listing skips the first image definition outside the maintenance window", func() {
				reconciler := closedWindowReconciler()
				_, err := reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(statusImageIDs()).To(Equal(fipsImageIDs(fipsVersion)))
				inUse := nodeClass.DeepCopy().Status.Images

				publishFIPSImages(newerFIPSVersion, 6)
				withFIPSImagesExcept(fipsDefinitions()[0])
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
				Expect(nodeClass.Status.Images).To(Equal(inUse))
			})

			DescribeTable("keeps the images in use, in their priority order, while the FIPS image definitions return one at a time outside the maintenance window",
				func(reverse bool) {
					reconciler := closedWindowReconciler()
					_, err := reconciler.Reconcile(ctx, nodeClass)
					Expect(err).ToNot(HaveOccurred())
					Expect(statusImageIDs()).To(Equal(fipsImageIDs(fipsVersion)))
					inUse := nodeClass.DeepCopy().Status.Images

					withoutFIPSImages()
					_, err = reconciler.Reconcile(ctx, nodeClass)
					Expect(err).ToNot(HaveOccurred())
					Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).Reason).To(Equal(imagefamily.FIPSImagesNotFoundReason))

					publishFIPSImages(newerFIPSVersion, 6)
					definitions := fipsDefinitions()
					if reverse {
						slices.Reverse(definitions)
					}
					for i, definition := range definitions {
						// The region lists the first i+1 image definitions in the order of their return, at newerFIPSVersion.
						withFIPSImagesExcept(definitions[i+1:]...)
						_, err = reconciler.Reconcile(ctx, nodeClass)
						Expect(err).ToNot(HaveOccurred())
						Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue(), "after %s returned", definition)
						Expect(nodeClass.Status.Images).To(Equal(inUse), "after %s returned", definition)
					}
				},
				Entry("in priority order", false),
				Entry("in reverse priority order", true),
			)

			It("lists a skipped image definition at its newest version when it returns after a pending update", func() {
				reconciler := closedWindowReconciler()
				_, err := reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())

				// A Kubernetes upgrade requests the newest images while the region lists only the first image definition.
				nodeClass.StatusConditions().SetFalse(v1beta1.ConditionTypeImagesReady, "KubernetesUpgrade", "Performing kubernetes upgrade, need to get latest images")
				publishFIPSImages(newerFIPSVersion, 6)
				withFIPSImagesExcept(fipsDefinitions()[1:]...)
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
				Expect(statusImageIDs()).To(ConsistOf(fipsImageIDs(newerFIPSVersion)[0]))

				withFIPSImagesExcept()
				_, err = reconciler.Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(statusImageIDs()).To(ConsistOf(fipsImageIDs(newerFIPSVersion)))
			})

			It("changes no image of a ready NodeClass while the maintenance window cannot be read, as before", func() {
				_, err := closedWindowReconciler().Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				ready := nodeClass.DeepCopy()

				withNewerFIPSImages()
				_, err = status.NewNodeImageReconciler(azureEnv.ImageProvider, unreadableWindow(), "kube-system").Reconcile(ctx, nodeClass)
				Expect(err).To(MatchError(ContainSubstring("forbidden")))
				Expect(nodeClass.Status).To(Equal(ready.Status))
			})

			DescribeTable("keeps the images in use and becomes ready when the FIPS images return while the maintenance window cannot be determined",
				func(maintenanceWindow func() kubernetes.Interface, problem string) {
					_, err := closedWindowReconciler().Reconcile(ctx, nodeClass)
					Expect(err).ToNot(HaveOccurred())
					inUse := nodeClass.DeepCopy().Status.Images

					withoutFIPSImages()
					_, err = closedWindowReconciler().Reconcile(ctx, nodeClass)
					Expect(err).ToNot(HaveOccurred())
					Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).Reason).To(Equal(imagefamily.FIPSImagesNotFoundReason))

					withNewerFIPSImages()
					_, err = status.NewNodeImageReconciler(azureEnv.ImageProvider, maintenanceWindow(), "kube-system").Reconcile(ctx, nodeClass)
					Expect(err).To(MatchError(And(ContainSubstring("checking maintenance window"), ContainSubstring(problem))))
					Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
					Expect(nodeClass.Status.Images).To(Equal(inUse))
				},
				Entry("when the ConfigMap cannot be read", unreadableWindow, "forbidden"),
				Entry("when the ConfigMap is incomplete", incompleteWindow, "incomplete maintenance window data"),
				Entry("when a time in the ConfigMap does not parse", unparseableWindow, "error parsing maintenance window start time"),
			)

			It("persists the images in use and ImagesReady through the status controller while the maintenance window cannot be read", func() {
				_, err := closedWindowReconciler().Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				inUse := nodeClass.DeepCopy().Status.Images

				withoutFIPSImages()
				_, err = closedWindowReconciler().Reconcile(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).Reason).To(Equal(imagefamily.FIPSImagesNotFoundReason))
				ExpectApplied(ctx, env.Client, nodeClass)

				withNewerFIPSImages()
				unreadableWindowController := status.NewController(env.Client, azureEnv.SubscriptionID, fake.Region, azureEnv.KubernetesVersionProvider, azureEnv.ImageProvider, unreadableWindow(), env.KubernetesInterface, azureEnv.DynamicInterface, azureEnv.SubnetsAPI, azureEnv.DiskEncryptionSetsAPI, testOptions.ParsedDiskEncryptionSetID, testOptions.NetworkPolicy, testOptions.NetworkPlugin,
					azureEnv.CapacityReservationGroupsAPI, azureEnv.CapacityReservationsAPI, azureEnv.InstanceTypesProvider, azureEnv.UnavailableOfferingsCache, "kube-system")
				_, err = unreadableWindowController.Reconcile(ctx, nodeClass)
				Expect(err).To(MatchError(And(ContainSubstring("checking maintenance window"), ContainSubstring("forbidden"))))
				stored := ExpectExists(ctx, env.Client, nodeClass)
				Expect(stored.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
				Expect(stored.Status.Images).To(Equal(inUse))
			})
		})
	})
})

func ExpectReadyWithImages(nodeClass *v1beta1.AKSNodeClass, version string, expectedImages func(string) []v1beta1.NodeImage) {
	GinkgoHelper()

	Expect(nodeClass.Status.Images).To(HaveExactElements(expectedImages(version)))
	Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
}
