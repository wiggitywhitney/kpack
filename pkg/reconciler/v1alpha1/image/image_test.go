package image_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
	duckv1alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
	"github.com/knative/pkg/kmeta"
	"github.com/sclevine/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/pivotal/build-service-system/pkg/reconciler/testhelpers"

	knCtrl "github.com/knative/pkg/controller"

	"github.com/pivotal/build-service-system/pkg/apis/build/v1alpha1"
	"github.com/pivotal/build-service-system/pkg/client/clientset/versioned/fake"
	"github.com/pivotal/build-service-system/pkg/client/informers/externalversions"
	v1build "github.com/pivotal/build-service-system/pkg/reconciler/v1alpha1/build"
	"github.com/pivotal/build-service-system/pkg/reconciler/v1alpha1/image"
)

func TestImageReconciler(t *testing.T) {
	spec.Run(t, "Image Reconciler", testImageReconciler)
}

func testImageReconciler(t *testing.T, when spec.G, it spec.S) {
	fakeClient := fake.NewSimpleClientset(&v1alpha1.Image{}, &v1alpha1.Builder{})

	fakeTracker := fakeTracker{}

	reconciler := testhelpers.RebuildingReconciler(func() knCtrl.Reconciler {
		informerFactory := externalversions.NewSharedInformerFactory(fakeClient, time.Second)
		imageInformer := informerFactory.Build().V1alpha1().Images()
		buildInformer := informerFactory.Build().V1alpha1().Builds()
		builderInformer := informerFactory.Build().V1alpha1().Builders()

		return testhelpers.SyncWaitingReconciler(
			informerFactory,
			&image.Reconciler{
				Client:        fakeClient,
				ImageLister:   imageInformer.Lister(),
				BuildLister:   buildInformer.Lister(),
				BuilderLister: builderInformer.Lister(),
				Tracker:       fakeTracker,
			},
			imageInformer.Informer().HasSynced,
			buildInformer.Informer().HasSynced,
			builderInformer.Informer().HasSynced,
		)
	})

	const (
		imageName                = "image-name"
		builderName              = "builder-name"
		namespace                = "some-namespace"
		key                      = "some-namespace/image-name"
		originalGeneration int64 = 1
	)
	var (
		failedBuildHistoryLimit  int64 = 4
		successBuildHistoryLimit int64 = 4
	)

	image := &v1alpha1.Image{
		ObjectMeta: v1.ObjectMeta{
			Name:       imageName,
			Namespace:  namespace,
			Generation: originalGeneration,
		},
		Spec: v1alpha1.ImageSpec{
			Image:          "some/image",
			ServiceAccount: "service-account",
			BuilderRef:     builderName,
			Source: v1alpha1.Source{
				Git: v1alpha1.Git{
					URL:      "https://some.git/url",
					Revision: "revision",
				},
			},
			FailedBuildHistoryLimit:  &failedBuildHistoryLimit,
			SuccessBuildHistoryLimit: &successBuildHistoryLimit,
		},
	}

	defaultBuildMetadata := v1alpha1.BuildpackMetadataList{
		{
			ID:      "buildpack.version",
			Version: "version",
		},
	}

	builder := &v1alpha1.Builder{
		ObjectMeta: v1.ObjectMeta{
			Name: builderName,
		},
		Spec: v1alpha1.BuilderSpec{
			Image: "some/builder@sha256acf123",
		},
		Status: v1alpha1.BuilderStatus{
			BuilderMetadata: defaultBuildMetadata,
		},
	}

	it.Before(func() {
		_, err := fakeClient.BuildV1alpha1().Builders(namespace).Create(builder)
		assert.Nil(t, err)
	})

	when("#Reconcile", func() {
		when("new image", func() {
			it.Before(func() {
				_, err := fakeClient.BuildV1alpha1().Images(namespace).Create(image)
				assert.Nil(t, err)
			})

			it("creates an initial Build", func() {
				err := reconciler.Reconcile(context.TODO(), key)
				assert.Nil(t, err)

				build, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
				assert.Nil(t, err)
				assert.Equal(t, len(build.Items), 1)

				buildName := build.Items[0].ObjectMeta.Name
				assert.Equal(t, build.Items[0], v1alpha1.Build{
					TypeMeta: v1.TypeMeta{},
					ObjectMeta: v1.ObjectMeta{
						Name:      buildName,
						Namespace: namespace,
						OwnerReferences: []v1.OwnerReference{
							*kmeta.NewControllerRef(image),
						},
						Labels: map[string]string{
							v1alpha1.BuildNumberLabel: "1",
							v1alpha1.ImageLabel:       imageName,
						},
					},
					Spec: v1alpha1.BuildSpec{
						Image:          "some/image",
						Builder:        "some/builder@sha256acf123",
						ServiceAccount: "service-account",
						Source: v1alpha1.Source{
							Git: v1alpha1.Git{
								URL:      "https://some.git/url",
								Revision: "revision",
							},
						},
					},
				})
				updatedImage, err := fakeClient.BuildV1alpha1().Images(namespace).Get(imageName, v1.GetOptions{})
				assert.Nil(t, err)
				assert.Equal(t, updatedImage.Status.LastBuildRef, buildName)
			})

			it("is not affected by other images", func() {
				differentImage := image.DeepCopy()
				differentImage.Name = "Different-Name"

				_, err := fakeClient.BuildV1alpha1().Images(namespace).Create(differentImage)
				assert.Nil(t, err)
				err = reconciler.Reconcile(context.TODO(), namespace+"/Different-Name")
				assert.Nil(t, err)

				err = reconciler.Reconcile(context.TODO(), key)
				assert.Nil(t, err)

				build, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{
					LabelSelector: "image.build.pivotal.io/image=" + imageName,
				})
				assert.Nil(t, err)
				assert.Equal(t, len(build.Items), 1)
			})

			it("updates the observed generation with the new spec", func() {
				err := reconciler.Reconcile(context.TODO(), key)
				assert.Nil(t, err)

				updatedImage, err := fakeClient.BuildV1alpha1().Images(namespace).Get(imageName, v1.GetOptions{})
				assert.Nil(t, err)
				assert.Equal(t, updatedImage.Status.ObservedGeneration, originalGeneration)
			})

			it("updates the build count", func() {
				err := reconciler.Reconcile(context.TODO(), key)
				assert.Nil(t, err)

				updatedImage, err := fakeClient.BuildV1alpha1().Images(namespace).Get(imageName, v1.GetOptions{})
				assert.Nil(t, err)
				assert.Equal(t, updatedImage.Status.BuildCounter, int32(1))
			})

			it("tracks the builder", func() {
				err := reconciler.Reconcile(context.TODO(), key)
				assert.Nil(t, err)

				updatedImage, err := fakeClient.BuildV1alpha1().Images(namespace).Get(imageName, v1.GetOptions{})
				assert.Nil(t, err)

				gvk := builder.GetGroupVersionKind()
				isTracking := fakeTracker.IsTracking(corev1.ObjectReference{
					APIVersion: gvk.GroupVersion().String(),
					Kind:       gvk.Kind,
					Namespace:  namespace,
					Name:       builderName,
				}, updatedImage)

				assert.True(t, isTracking)
			})
		})

		when("a build has already been created", func() {
			it.Before(func() {
				_, err := fakeClient.BuildV1alpha1().Images(namespace).Create(image)
				assert.Nil(t, err)

				err = reconciler.Reconcile(context.TODO(), key)
				assert.Nil(t, err)
			})

			when("a new spec is applied", func() {
				const newGeneration int64 = 2

				it.Before(func() {
					img, err := fakeClient.BuildV1alpha1().Images(namespace).Get(imageName, v1.GetOptions{})
					assert.Nil(t, err)

					_, err = fakeClient.BuildV1alpha1().Images(namespace).Update(&v1alpha1.Image{
						ObjectMeta: v1.ObjectMeta{
							Name:       imageName,
							Generation: newGeneration,
						},
						Spec: v1alpha1.ImageSpec{
							Image:          "different/image",
							ServiceAccount: "different/service-account",
							BuilderRef:     builderName,
							Source: v1alpha1.Source{
								Git: v1alpha1.Git{
									URL:      "https://different.git/url",
									Revision: "differentrevision",
								},
							},
							FailedBuildHistoryLimit:  &failedBuildHistoryLimit,
							SuccessBuildHistoryLimit: &successBuildHistoryLimit,
						},
						Status: img.Status, // fake client overwrites status :(
					})
					assert.Nil(t, err)
				})

				it("does not create a build when a build is running", func() {
					updateStatusOfLastBuild(t, fakeClient, namespace, nil, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionUnknown,
					})

					err := reconciler.Reconcile(context.TODO(), key)
					assert.Nil(t, err)

					builds, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
					assert.Nil(t, err)
					assert.Equal(t, len(builds.Items), 1)

					updatedImage, err := fakeClient.BuildV1alpha1().Images(namespace).Get(imageName, v1.GetOptions{})
					assert.Nil(t, err)
					assert.Equal(t, updatedImage.Status.ObservedGeneration, originalGeneration)
				})

				it("does create a build when the last build is successful", func() {
					updateStatusOfLastBuild(t, fakeClient, namespace, defaultBuildMetadata, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionTrue,
					})

					err := reconciler.Reconcile(context.TODO(), key)
					assert.Nil(t, err)

					builds, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
					assert.Nil(t, err)
					assert.Equal(t, len(builds.Items), 2)

					updatedImage, err := fakeClient.BuildV1alpha1().Images(namespace).Get(imageName, v1.GetOptions{})
					assert.Nil(t, err)
					assert.Equal(t, updatedImage.Status.ObservedGeneration, newGeneration)

					newBuild, err := fakeClient.BuildV1alpha1().Builds(namespace).Get(updatedImage.Status.LastBuildRef, v1.GetOptions{})
					assert.Nil(t, err)
					assert.Equal(t, newBuild, &v1alpha1.Build{
						TypeMeta: v1.TypeMeta{},
						ObjectMeta: v1.ObjectMeta{
							Name:      updatedImage.Status.LastBuildRef,
							Namespace: namespace,
							OwnerReferences: []v1.OwnerReference{
								*kmeta.NewControllerRef(image),
							},
							Labels: map[string]string{
								v1alpha1.BuildNumberLabel: "2",
								v1alpha1.ImageLabel:       imageName,
							},
						},
						Spec: v1alpha1.BuildSpec{
							Image:          "different/image",
							ServiceAccount: "different/service-account",
							Builder:        "some/builder@sha256acf123",
							Source: v1alpha1.Source{
								Git: v1alpha1.Git{
									URL:      "https://different.git/url",
									Revision: "differentrevision",
								},
							},
						},
					})
				})

				it("does create a build when the last build is a failure", func() {
					updateStatusOfLastBuild(t, fakeClient, namespace, nil, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionFalse,
					})

					err := reconciler.Reconcile(context.TODO(), key)
					assert.Nil(t, err)

					builds, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
					assert.Nil(t, err)
					assert.Equal(t, len(builds.Items), 2)
				})
			})

			when("referenced builder has been updated", func() {
				it.Before(func() {
					_, err := fakeClient.BuildV1alpha1().Builders(namespace).Update(&v1alpha1.Builder{
						ObjectMeta: v1.ObjectMeta{
							Name: builderName,
						},
						Spec: v1alpha1.BuilderSpec{
							Image: "some/builder@sha256:newsha",
						},
						Status: v1alpha1.BuilderStatus{
							BuilderMetadata: []v1alpha1.BuildpackMetadata{
								{
									ID:      "new.buildpack",
									Version: "version",
								},
							},
						},
					})
					assert.Nil(t, err)
				})

				it("does not create a build when a build is running", func() {
					updateStatusOfLastBuild(t, fakeClient, namespace, nil, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionUnknown,
					})

					err := reconciler.Reconcile(context.TODO(), key)
					assert.Nil(t, err)

					builds, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
					assert.Nil(t, err)
					assert.Equal(t, len(builds.Items), 1)
				})

				it("does create a build when the last build is no longer running", func() {
					updateStatusOfLastBuild(t, fakeClient, namespace, defaultBuildMetadata, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionTrue,
					})

					err := reconciler.Reconcile(context.TODO(), key)
					assert.Nil(t, err)

					builds, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
					assert.Nil(t, err)
					assert.Equal(t, len(builds.Items), 2)

					updatedImage, err := fakeClient.BuildV1alpha1().Images(namespace).Get(imageName, v1.GetOptions{})
					assert.Nil(t, err)

					newBuild, err := fakeClient.BuildV1alpha1().Builds(namespace).Get(updatedImage.Status.LastBuildRef, v1.GetOptions{})
					assert.Nil(t, err)
					assert.Equal(t, newBuild, &v1alpha1.Build{
						TypeMeta: v1.TypeMeta{},
						ObjectMeta: v1.ObjectMeta{
							Name:      updatedImage.Status.LastBuildRef,
							Namespace: namespace,
							OwnerReferences: []v1.OwnerReference{
								*kmeta.NewControllerRef(image),
							},
							Labels: map[string]string{
								v1alpha1.BuildNumberLabel: "2",
								v1alpha1.ImageLabel:       imageName,
							},
						},
						Spec: v1alpha1.BuildSpec{
							Image:          "some/image",
							Builder:        "some/builder@sha256:newsha",
							ServiceAccount: "service-account",
							Source: v1alpha1.Source{
								Git: v1alpha1.Git{
									URL:      "https://some.git/url",
									Revision: "revision",
								},
							},
						},
					})
				})
			})

			when("no new spec has been applied", func() {
				it("does not create a new build when last build is running", func() {
					updateStatusOfLastBuild(t, fakeClient, namespace, nil, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionUnknown,
					})

					err := reconciler.Reconcile(context.TODO(), key)
					assert.Nil(t, err)

					builds, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
					assert.Nil(t, err)
					assert.Equal(t, len(builds.Items), 1)
				})

				it("does not create a build when the last build is no longer running", func() {
					updateStatusOfLastBuild(t, fakeClient, namespace, defaultBuildMetadata, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionTrue,
					})

					err := reconciler.Reconcile(context.TODO(), key)
					assert.Nil(t, err)

					builds, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
					assert.Nil(t, err)
					assert.Equal(t, len(builds.Items), 1)
				})
			})

		})

		when("an image status is not up to date", func() {
			it.Before(func() {
				_, err := fakeClient.BuildV1alpha1().Images(namespace).Create(image)
				assert.Nil(t, err)
			})

			it("does not create duplicate builds", func() {
				_, err := fakeClient.BuildV1alpha1().Builds(namespace).Create(&v1alpha1.Build{
					ObjectMeta: v1.ObjectMeta{
						Name: "gotprocessed-beforeimage-saved",
						Labels: map[string]string{
							v1alpha1.BuildNumberLabel: "1",
							v1alpha1.ImageLabel:       imageName,
						},
					},
					Spec:   v1alpha1.BuildSpec{},
					Status: v1alpha1.BuildStatus{},
				})
				assert.Nil(t, err)

				err = reconciler.Reconcile(context.TODO(), key)
				assert.Error(t, err, fmt.Sprintf("warning: image %s status not up to date", imageName))

				build, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
				assert.Nil(t, err)

				assert.Equal(t, len(build.Items), 1)
			})
		})

		when("failed builds have exceeded the failedHistoryLimit", func() {
			var firstBuild *v1alpha1.Build

			it.Before(func() {
				_, err := fakeClient.BuildV1alpha1().Images(namespace).Create(image)
				require.Nil(t, err)

				for i := int64(0); i < failedBuildHistoryLimit+1; i++ {
					build := image.CreateBuild(builder)
					build.ObjectMeta.CreationTimestamp = v1.NewTime(time.Now().Add(time.Duration(i) * time.Minute))
					_, err = fakeClient.BuildV1alpha1().Builds(namespace).Create(build)
					require.Nil(t, err)

					updateStatusOfLastBuild(t, fakeClient, namespace, nil, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionFalse,
					})
					if firstBuild == nil {
						firstBuild = build
					}
					image.Status.BuildCounter++
					image.Status.LastBuildRef = build.Name
					_, err = fakeClient.BuildV1alpha1().Images(namespace).UpdateStatus(image)
					assert.Nil(t, err)
				}
			})

			it("removes failed builds over the limit", func() {
				err := reconciler.Reconcile(context.TODO(), key)
				require.NoError(t, err)

				buildList, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
				require.NoError(t, err)

				require.Len(t, buildList.Items, int(failedBuildHistoryLimit))

				_, err = fakeClient.BuildV1alpha1().Builds(namespace).Get(firstBuild.Name, v1.GetOptions{})
				require.Error(t, err, "not found")
			})

		})

		when("success builds have exceeded the successHistoryLimit", func() {
			var firstBuild *v1alpha1.Build
			it.Before(func() {
				_, err := fakeClient.BuildV1alpha1().Images(namespace).Create(image)
				require.Nil(t, err)

				for i := int64(0); i < successBuildHistoryLimit+1; i++ {
					build := image.CreateBuild(builder)
					build.ObjectMeta.CreationTimestamp = v1.NewTime(time.Now().Add(time.Duration(i) * time.Minute))
					_, err = fakeClient.BuildV1alpha1().Builds(namespace).Create(build)
					require.Nil(t, err)

					if firstBuild == nil {
						firstBuild = build
					}

					updateStatusOfLastBuild(t, fakeClient, namespace, nil, duckv1alpha1.Condition{
						Type:   duckv1alpha1.ConditionSucceeded,
						Status: corev1.ConditionTrue,
					})

					image.Status.BuildCounter++
					image.Status.LastBuildRef = build.Name
					_, err = fakeClient.BuildV1alpha1().Images(namespace).UpdateStatus(image)
					assert.Nil(t, err)
				}
			})

			it("removes success builds over the limit", func() {

				err := reconciler.Reconcile(context.TODO(), key)
				require.NoError(t, err)

				buildList, err := fakeClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
				require.NoError(t, err)

				require.Len(t, buildList.Items, int(successBuildHistoryLimit))

				_, err = fakeClient.BuildV1alpha1().Builds(namespace).Get(firstBuild.Name, v1.GetOptions{})
				require.Error(t, err, "not found")
			})
		})

		it("does not return error on nonexistent image", func() {
			err := reconciler.Reconcile(context.TODO(), "not/found")
			assert.Nil(t, err)
		})

	})
}

func updateStatusOfLastBuild(t *testing.T, fakeImageClient *fake.Clientset, namespace string, buildMetadata v1alpha1.BuildpackMetadataList, condition duckv1alpha1.Condition) v1alpha1.Build {
	build, err := fakeImageClient.BuildV1alpha1().Builds(namespace).List(v1.ListOptions{})
	assert.Nil(t, err)
	var itemList []*v1alpha1.Build
	for _, value := range build.Items {
		itemList = append(itemList, &value)
	}
	sort.Sort(v1build.ByCreationTimestamp(itemList))

	lastBuild := itemList[len(itemList)-1]
	_, err = fakeImageClient.BuildV1alpha1().Builds(namespace).UpdateStatus(&v1alpha1.Build{
		ObjectMeta: lastBuild.ObjectMeta,
		Spec:       lastBuild.Spec, // fake client overwrites spec :(
		Status: v1alpha1.BuildStatus{
			Status: alpha1.Status{
				Conditions: alpha1.Conditions{
					condition,
				},
			},
			BuildMetadata: buildMetadata,
		},
	})
	assert.Nil(t, err)
	return *lastBuild
}

type fakeTracker map[corev1.ObjectReference]map[string]struct{}

func (f fakeTracker) Track(ref corev1.ObjectReference, obj interface{}) error {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		return err
	}

	_, ok := f[ref]
	if !ok {
		f[ref] = map[string]struct{}{}
	}

	f[ref][key] = struct{}{}
	return nil
}

func (fakeTracker) OnChanged(obj interface{}) {
	panic("I should not be called in tests")
}

func (f fakeTracker) IsTracking(ref corev1.ObjectReference, obj interface{}) bool {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		return false
	}

	trackingObs, ok := f[ref]
	if !ok {
		return false
	}
	_, ok = trackingObs[key]

	return ok
}