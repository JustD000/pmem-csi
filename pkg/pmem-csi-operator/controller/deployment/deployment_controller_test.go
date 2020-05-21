/*
Copyright 2019  Intel Corporation.

SPDX-License-Identifier: Apache-2.0
*/
package deployment_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	// "github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/intel/pmem-csi/deploy"
	"github.com/intel/pmem-csi/pkg/apis"
	api "github.com/intel/pmem-csi/pkg/apis/pmemcsi/v1alpha1"
	pmemcontroller "github.com/intel/pmem-csi/pkg/pmem-csi-operator/controller"
	"github.com/intel/pmem-csi/pkg/pmem-csi-operator/controller/deployment"
	"github.com/intel/pmem-csi/pkg/pmem-csi-operator/controller/deployment/testcases"
	pmemtls "github.com/intel/pmem-csi/pkg/pmem-csi-operator/pmem-tls"
	"github.com/intel/pmem-csi/pkg/version"
	"github.com/intel/pmem-csi/test/e2e/operator/validate"

	corev1 "k8s.io/api/core/v1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type pmemDeployment struct {
	name                                                string
	deviceMode                                          string
	logLevel                                            uint16
	image, pullPolicy, provisionerImage, registrarImage string
	controllerCPU, controllerMemory                     string
	nodeCPU, nodeMemory                                 string
	caCert, regCert, regKey, ncCert, ncKey              []byte
}

func getDeployment(d *pmemDeployment) *api.Deployment {
	dep := &api.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "pmem-csi.intel.com/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: d.name,
			UID:  types.UID("fake-uuid-" + d.name),
		},
	}

	// TODO (?): embed DeploymentSpec inside pmemDeployment instead of splitting it up into individual values.
	// The entire copying block below then collapses into a single line.

	dep.Spec = api.DeploymentSpec{}
	spec := &dep.Spec
	spec.DeviceMode = api.DeviceMode(d.deviceMode)
	spec.LogLevel = d.logLevel
	spec.Image = d.image
	spec.PullPolicy = corev1.PullPolicy(d.pullPolicy)
	spec.ProvisionerImage = d.provisionerImage
	spec.NodeRegistrarImage = d.registrarImage
	if d.controllerCPU != "" || d.controllerMemory != "" {
		spec.ControllerResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(d.controllerCPU),
				corev1.ResourceMemory: resource.MustParse(d.controllerMemory),
			},
		}
	}
	if d.nodeCPU != "" || d.nodeMemory != "" {
		spec.NodeResources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(d.nodeCPU),
				corev1.ResourceMemory: resource.MustParse(d.nodeMemory),
			},
		}
	}
	spec.CACert = d.caCert
	spec.RegistryCert = d.regCert
	spec.RegistryPrivateKey = d.regKey
	spec.NodeControllerCert = d.ncCert
	spec.NodeControllerPrivateKey = d.ncKey

	return dep
}

func testDeploymentPhase(t *testing.T, c client.Client, name string, expectedPhase api.DeploymentPhase) {
	depObject := &api.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	err := c.Get(context.TODO(), namespacedNameWithOffset(t, 3, depObject), depObject)
	require.NoError(t, err, "failed to retrive deployment object")
	require.Equal(t, expectedPhase, depObject.Status.Phase, "Unexpected status phase")
}

func testReconcile(t *testing.T, rc reconcile.Reconciler, name string, expectErr bool, expectedRequeue bool) {
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name: name,
		},
	}
	resp, err := rc.Reconcile(req)
	if expectErr {
		require.Error(t, err, "expected reconcile failure")
	} else {
		require.NoError(t, err, "reconcile failed with error")
	}
	require.Equal(t, expectedRequeue, resp.Requeue, "expected requeue reconcile")
}

func testReconcilePhase(t *testing.T, rc reconcile.Reconciler, c client.Client, name string, expectErr bool, expectedRequeue bool, expectedPhase api.DeploymentPhase) {
	testReconcile(t, rc, name, expectErr, expectedRequeue)
	testDeploymentPhase(t, c, name, expectedPhase)
}

func namespacedName(t *testing.T, obj runtime.Object) types.NamespacedName {
	return namespacedNameWithOffset(t, 2, obj)
}

func namespacedNameWithOffset(t *testing.T, offset int, obj runtime.Object) types.NamespacedName {
	metaObj, err := meta.Accessor(obj)
	require.NoError(t, err, "failed to get accessor")

	return types.NamespacedName{Name: metaObj.GetName(), Namespace: metaObj.GetNamespace()}
}

// objectKey creates the lookup key for an object with a name and optionally with a namespace.
func objectKey(name string, namespace ...string) client.ObjectKey {
	key := types.NamespacedName{
		Name: name,
	}
	if len(namespace) > 0 {
		key.Namespace = namespace[0]
	}
	return key
}

func deleteDeployment(c client.Client, name, ns string) error {
	dep := &api.Deployment{}
	key := objectKey(name)
	if err := c.Get(context.TODO(), key, dep); err != nil {
		return err
	}

	if err := c.Delete(context.TODO(), dep); err != nil {
		return err
	}
	// Delete sub-objects created by this deployment which
	// are possible might conflicts(CSIDriver) with later part of test
	// This is supposed to handle by Kubernetes grabage collector
	// but couldn't provided by fake client the tets are using
	//
	driver := &storagev1beta1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: dep.Name,
		},
	}
	return c.Delete(context.TODO(), driver)
}

func TestDeploymentController(t *testing.T) {
	err := apis.AddToScheme(scheme.Scheme)
	require.NoError(t, err, "add api schema")

	testIt := func(t *testing.T, testK8sVersion version.Version) {
		type testContext struct {
			c  client.Client
			rc reconcile.Reconciler
		}
		const (
			testNamespace   = "test-namespace"
			testDriverImage = "fake-driver-image"
		)

		setup := func(t *testing.T) testContext {
			c := fake.NewFakeClient()
			rc, err := deployment.NewReconcileDeployment(c, pmemcontroller.ControllerOptions{
				Namespace:   testNamespace,
				K8sVersion:  testK8sVersion,
				DriverImage: testDriverImage,
			})
			require.NoError(t, err, "create new reconciler")
			return testContext{c, rc}
		}

		validateDriver := func(t *testing.T, tc testContext, dep *api.Deployment) {
			// We may have to fill in some defaults, so make a copy first.
			dep = dep.DeepCopyObject().(*api.Deployment)
			if dep.Spec.Image == "" {
				dep.Spec.Image = testDriverImage
			}

			require.NoError(t, validate.DriverDeployment(tc.c, testK8sVersion, testNamespace, *dep), "validate deployment")
		}

		t.Parallel()

		t.Run("deployment with defaults", func(t *testing.T) {
			tc := setup(t)
			d := &pmemDeployment{
				name: "test-deployment",
			}

			dep := getDeployment(d)

			err := tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment")

			testReconcilePhase(t, tc.rc, tc.c, d.name, false, false, api.DeploymentPhaseRunning)
			validateDriver(t, tc, dep)
		})

		t.Run("deployment with explicit values", func(t *testing.T) {
			tc := setup(t)
			d := &pmemDeployment{
				name:             "test-deployment",
				image:            "test-driver:v0.0.0",
				provisionerImage: "test-provisioner-image:v0.0.0",
				registrarImage:   "test-driver-registrar-image:v.0.0.0",
				pullPolicy:       "Never",
				logLevel:         10,
				controllerCPU:    "1500m",
				controllerMemory: "300Mi",
				nodeCPU:          "1000m",
				nodeMemory:       "500Mi",
			}

			dep := getDeployment(d)
			err := tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment")

			// Reconcile now should change Phase to running
			testReconcilePhase(t, tc.rc, tc.c, d.name, false, false, api.DeploymentPhaseRunning)
			validateDriver(t, tc, dep)
		})

		t.Run("multiple deployments", func(t *testing.T) {
			tc := setup(t)
			d1 := &pmemDeployment{
				name: "test-deployment1",
			}

			d2 := &pmemDeployment{
				name: "test-deployment2",
			}

			dep := getDeployment(d1)
			err := tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment1")

			dep = getDeployment(d2)
			err = tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment2")

			testReconcilePhase(t, tc.rc, tc.c, d1.name, false, false, api.DeploymentPhaseRunning)
			testReconcilePhase(t, tc.rc, tc.c, d2.name, false, false, api.DeploymentPhaseRunning)
		})

		t.Run("invalid device mode", func(t *testing.T) {
			tc := setup(t)
			d := &pmemDeployment{
				name:       "test-driver-modes",
				deviceMode: "foobar",
			}

			dep := getDeployment(d)

			err := tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment")
			// Deployment should failed with an error
			testReconcilePhase(t, tc.rc, tc.c, d.name, true, false, api.DeploymentPhaseFailed)
		})

		t.Run("provided private keys", func(t *testing.T) {
			tc := setup(t)
			// Generate private key
			regKey, err := pmemtls.NewPrivateKey()
			require.NoError(t, err, "Failed to generate a private key: %v", err)

			encodedKey := pmemtls.EncodeKey(regKey)

			d := &pmemDeployment{
				name:   "test-deployment",
				regKey: encodedKey,
			}
			dep := getDeployment(d)
			err = tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment")

			// First deployment expected to be successful
			testReconcilePhase(t, tc.rc, tc.c, d.name, false, false, api.DeploymentPhaseRunning)
			validateDriver(t, tc, dep)
		})

		t.Run("provided private keys and certificates", func(t *testing.T) {
			tc := setup(t)
			ca, err := pmemtls.NewCA(nil, nil)
			require.NoError(t, err, "faield to instantiate CA")

			regKey, err := pmemtls.NewPrivateKey()
			require.NoError(t, err, "failed to generate a private key: %v", err)
			regCert, err := ca.GenerateCertificate("pmem-registry", regKey.Public())
			require.NoError(t, err, "failed to sign registry key")

			ncKey, err := pmemtls.NewPrivateKey()
			require.NoError(t, err, "failed to generate a private key: %v", err)
			ncCert, err := ca.GenerateCertificate("pmem-node-controller", ncKey.Public())
			require.NoError(t, err, "failed to sign node controller key")

			d := &pmemDeployment{
				name:    "test-deployment",
				caCert:  ca.EncodedCertificate(),
				regKey:  pmemtls.EncodeKey(regKey),
				regCert: pmemtls.EncodeCert(regCert),
				ncKey:   pmemtls.EncodeKey(ncKey),
				ncCert:  pmemtls.EncodeCert(ncCert),
			}
			dep := getDeployment(d)
			err = tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment")

			// First deployment expected to be successful
			testReconcilePhase(t, tc.rc, tc.c, d.name, false, false, api.DeploymentPhaseRunning)
			validateDriver(t, tc, dep)
		})

		t.Run("invalid private keys and certificates", func(t *testing.T) {
			tc := setup(t)
			ca, err := pmemtls.NewCA(nil, nil)
			require.NoError(t, err, "faield to instantiate CA")

			regKey, err := pmemtls.NewPrivateKey()
			require.NoError(t, err, "failed to generate a private key: %v", err)
			regCert, err := ca.GenerateCertificate("invalid-registry", regKey.Public())
			require.NoError(t, err, "failed to sign registry key")

			ncKey, err := pmemtls.NewPrivateKey()
			require.NoError(t, err, "failed to generate a private key: %v", err)
			ncCert, err := ca.GenerateCertificate("invalid-node-controller", ncKey.Public())
			require.NoError(t, err, "failed to sign node key")

			d := &pmemDeployment{
				name:    "test-deployment-cert-invalid",
				caCert:  ca.EncodedCertificate(),
				regKey:  pmemtls.EncodeKey(regKey),
				regCert: pmemtls.EncodeCert(regCert),
				ncKey:   pmemtls.EncodeKey(ncKey),
				ncCert:  pmemtls.EncodeCert(ncCert),
			}
			dep := getDeployment(d)
			err = tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment")

			testReconcilePhase(t, tc.rc, tc.c, d.name, true, true, api.DeploymentPhaseFailed)
		})

		t.Run("expired certificates", func(t *testing.T) {
			tc := setup(t)
			oneDayAgo := time.Now().Add(-24 * time.Hour)
			oneMinuteAgo := time.Now().Add(-1 * time.Minute)

			ca, err := pmemtls.NewCA(nil, nil)
			require.NoError(t, err, "faield to instantiate CA")

			regKey, err := pmemtls.NewPrivateKey()
			require.NoError(t, err, "failed to generate a private key: %v", err)
			regCert, err := ca.GenerateCertificateWithDuration("pmem-registry", oneDayAgo, oneMinuteAgo, regKey.Public())
			require.NoError(t, err, "failed to registry sign key")

			ncKey, err := pmemtls.NewPrivateKey()
			require.NoError(t, err, "failed to generate a private key: %v", err)
			ncCert, err := ca.GenerateCertificateWithDuration("pmem-node-controller", oneDayAgo, oneMinuteAgo, ncKey.Public())
			require.NoError(t, err, "failed to sign node controller key")

			d := &pmemDeployment{
				name:    "test-deployment-cert-expired",
				caCert:  ca.EncodedCertificate(),
				regKey:  pmemtls.EncodeKey(regKey),
				regCert: pmemtls.EncodeCert(regCert),
				ncKey:   pmemtls.EncodeKey(ncKey),
				ncCert:  pmemtls.EncodeCert(ncCert),
			}
			dep := getDeployment(d)
			err = tc.c.Create(context.TODO(), dep)
			require.NoError(t, err, "failed to create deployment")

			testReconcilePhase(t, tc.rc, tc.c, d.name, true, true, api.DeploymentPhaseFailed)
		})

		t.Run("updating", func(t *testing.T) {
			t.Parallel()
			for _, testcase := range testcases.UpdateTests() {
				testcase := testcase
				t.Run(testcase.Name, func(t *testing.T) {
					testIt := func(restart bool) {
						tc := setup(t)
						dep := testcase.Deployment.DeepCopyObject().(*api.Deployment)

						// When working with the fake client, we need to make up a UID.
						dep.UID = types.UID("fake-uid-" + dep.Name)

						err := tc.c.Create(context.TODO(), dep)
						require.NoError(t, err, "create deployment")

						testReconcilePhase(t, tc.rc, tc.c, dep.Name, false, false, api.DeploymentPhaseRunning)
						validateDriver(t, tc, dep)

						// Reconcile now should keep phase as running.
						testReconcilePhase(t, tc.rc, tc.c, dep.Name, false, false, api.DeploymentPhaseRunning)

						// Retrieve existing object before updating it.
						err = tc.c.Get(context.TODO(), types.NamespacedName{Name: dep.Name}, dep)
						require.NoError(t, err, "retrive existing deployment object")

						if restart {
							// Simulate restarting the operator by creating a new instance.
							rc, err := deployment.NewReconcileDeployment(tc.c, pmemcontroller.ControllerOptions{
								Namespace:   testNamespace,
								K8sVersion:  testK8sVersion,
								DriverImage: testDriverImage,
							})
							require.NoError(t, err, "recreate reconciler")
							tc.rc = rc
						}

						// Update.
						testcase.Mutate(dep)
						err = tc.c.Update(context.TODO(), dep)
						require.NoError(t, err, "update deployment")

						// Reconcile is expected to not fail.
						testReconcilePhase(t, tc.rc, tc.c, dep.Name, false, false, api.DeploymentPhaseRunning)

						// Recheck the container resources are updated
						validateDriver(t, tc, dep)
					}

					t.Run("while running", func(t *testing.T) {
						testIt(false)
					})

					t.Run("while stopped", func(t *testing.T) {
						testIt(true)
					})
				})
			}
		})
	}

	t.Parallel()

	// Validate for all supported Kubernetes versions.
	for _, yaml := range deploy.ListAll() {
		version := yaml.Kubernetes
		t.Run(fmt.Sprintf("Kubernetes %v", version), func(t *testing.T) {
			testIt(t, version)
		})
	}
}
