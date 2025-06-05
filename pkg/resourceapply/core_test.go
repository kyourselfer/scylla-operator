package resourceapply

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/scylladb/scylla-operator/pkg/pointer"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	apimachineryutilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

func TestApplyService(t *testing.T) {
	// Using a generating function prevents unwanted mutations.
	newService := func() *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test",
				Labels:    map[string]string{},
				OwnerReferences: []metav1.OwnerReference{
					{
						Controller:         pointer.Ptr(true),
						UID:                "abcdefgh",
						APIVersion:         "scylla.scylladb.com/v1",
						Kind:               "ScyllaCluster",
						Name:               "basic",
						BlockOwnerDeletion: pointer.Ptr(true),
					},
				},
			},
			Spec: corev1.ServiceSpec{},
		}
	}

	newServiceWithHash := func() *corev1.Service {
		svc := newService()
		apimachineryutilruntime.Must(SetHashAnnotation(svc))
		return svc
	}

	tt := []struct {
		name            string
		existing        []runtime.Object
		cache           []runtime.Object // nil cache means autofill from the client
		required        *corev1.Service
		forceOwnership  bool
		expectedService *corev1.Service
		expectedChanged bool
		expectedErr     error
		expectedEvents  []string
	}{
		{
			name:            "creates a new service when there is none",
			existing:        nil,
			required:        newService(),
			expectedService: newServiceWithHash(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceCreated Service default/test created"},
		},
		{
			name: "does nothing if the same service already exists",
			existing: []runtime.Object{
				newServiceWithHash(),
			},
			required:        newService(),
			expectedService: newServiceWithHash(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "does nothing if the same service already exists and required one has the hash",
			existing: []runtime.Object{
				newServiceWithHash(),
			},
			required:        newServiceWithHash(),
			expectedService: newServiceWithHash(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "updates the service if it exists without the hash",
			existing: []runtime.Object{
				newService(),
			},
			required:        newService(),
			expectedService: newServiceWithHash(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceUpdated Service default/test updated"},
		},
		{
			name:     "fails to create the service without a controllerRef",
			existing: nil,
			required: func() *corev1.Service {
				svc := newService()
				svc.OwnerReferences = nil
				return svc
			}(),
			expectedService: nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Service "default/test" is missing controllerRef`),
			expectedEvents:  nil,
		},
		{
			name: "updates the service if ports differ",
			existing: []runtime.Object{
				newService(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
					Name: "https",
				})
				return svc
			}(),
			expectedService: func() *corev1.Service {
				svc := newService()
				svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
					Name: "https",
				})
				apimachineryutilruntime.Must(SetHashAnnotation(svc))
				return svc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceUpdated Service default/test updated"},
		},
		{
			name: "updates the service if labels differ",
			existing: []runtime.Object{
				newServiceWithHash(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Labels["foo"] = "bar"
				return svc
			}(),
			expectedService: func() *corev1.Service {
				svc := newService()
				svc.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(svc))
				return svc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceUpdated Service default/test updated"},
		},
		{
			name: "won't update the service if an admission changes the sts",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newServiceWithHash()
					// Simulate admission by changing a value after the hash is computed.
					svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
						Name: "admissionchange",
					})
					return svc
				}(),
			},
			required: newService(),
			expectedService: func() *corev1.Service {
				svc := newServiceWithHash()
				// Simulate admission by changing a value after the hash is computed.
				svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
					Name: "admissionchange",
				})
				return svc
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			// We test propagating the RV from required in all the other tests.
			name: "specifying no RV will use the one from the existing object",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newServiceWithHash()
					svc.ResourceVersion = "21"
					return svc
				}(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.ResourceVersion = ""
				svc.Labels["foo"] = "bar"
				return svc
			}(),
			expectedService: func() *corev1.Service {
				svc := newService()
				svc.ResourceVersion = "21"
				svc.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(svc))
				return svc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceUpdated Service default/test updated"},
		},
		{
			name:     "update fails if the service is missing but we still see it in the cache",
			existing: nil,
			cache: []runtime.Object{
				newServiceWithHash(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Labels["foo"] = "bar"
				return svc
			}(),
			expectedService: nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`can't update /v1, Kind=Service "default/test": %w`, apierrors.NewNotFound(corev1.Resource("services"), "test")),
			expectedEvents:  []string{`Warning UpdateServiceFailed Failed to update Service default/test: services "test" not found`},
		},
		{
			name: "update fails if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newService()
					svc.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(svc))
					return svc
				}(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Labels["foo"] = "bar"
				return svc
			}(),
			expectedService: nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Service "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateServiceFailed Failed to update Service default/test: /v1, Kind=Service "default/test" isn't controlled by us`},
		},
		{
			name: "forced update succeeds if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newService()
					svc.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(svc))
					return svc
				}(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Labels["foo"] = "bar"
				return svc
			}(),
			forceOwnership: true,
			expectedService: func() *corev1.Service {
				svc := newService()
				svc.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(svc))
				return svc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceUpdated Service default/test updated"},
		},
		{
			name: "update succeeds to replace ownerRef kind",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newService()
					svc.OwnerReferences[0].Kind = "WrongKind"
					apimachineryutilruntime.Must(SetHashAnnotation(svc))
					return svc
				}(),
			},
			required: func() *corev1.Service {
				svc := newService()
				return svc
			}(),
			expectedService: func() *corev1.Service {
				svc := newService()
				apimachineryutilruntime.Must(SetHashAnnotation(svc))
				return svc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceUpdated Service default/test updated"},
		},
		{
			name: "update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newService()
					svc.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(svc))
					return svc
				}(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Labels["foo"] = "bar"
				return svc
			}(),
			expectedService: nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Service "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateServiceFailed Failed to update Service default/test: /v1, Kind=Service "default/test" isn't controlled by us`},
		},
		{
			name: "forced update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newService()
					svc.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(svc))
					return svc
				}(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Labels["foo"] = "bar"
				return svc
			}(),
			forceOwnership:  true,
			expectedService: nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Service "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateServiceFailed Failed to update Service default/test: /v1, Kind=Service "default/test" isn't controlled by us`},
		},
		{
			name: "all label and annotation keys are kept when the hash matches",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newService()
					svc.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "",
					}
					svc.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(svc))
					svc.Annotations["a-1"] = "a-alpha-changed"
					svc.Annotations["a-3"] = "a-resurrected"
					svc.Annotations["a-custom"] = "custom-value"
					svc.Labels["l-1"] = "l-alpha-changed"
					svc.Labels["l-3"] = "l-resurrected"
					svc.Labels["l-custom"] = "custom-value"
					return svc
				}(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				svc.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				return svc
			}(),
			forceOwnership: false,
			expectedService: func() *corev1.Service {
				svc := newService()
				svc.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				svc.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(svc))
				svc.Annotations["a-1"] = "a-alpha-changed"
				svc.Annotations["a-3"] = "a-resurrected"
				svc.Annotations["a-custom"] = "custom-value"
				svc.Labels["l-1"] = "l-alpha-changed"
				svc.Labels["l-3"] = "l-resurrected"
				svc.Labels["l-custom"] = "custom-value"
				return svc
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "only managed label and annotation keys are updated when the hash changes",
			existing: []runtime.Object{
				func() *corev1.Service {
					svc := newService()
					svc.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "a-resurrected",
					}
					svc.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "l-resurrected",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(svc))
					svc.Annotations["a-1"] = "a-alpha-changed"
					svc.Annotations["a-custom"] = "a-custom-value"
					svc.Labels["l-1"] = "l-alpha-changed"
					svc.Labels["l-custom"] = "l-custom-value"
					return svc
				}(),
			},
			required: func() *corev1.Service {
				svc := newService()
				svc.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				svc.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				return svc
			}(),
			forceOwnership: true,
			expectedService: func() *corev1.Service {
				svc := newService()
				svc.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				svc.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(svc))
				delete(svc.Annotations, "a-3-")
				svc.Annotations["a-custom"] = "a-custom-value"
				delete(svc.Labels, "l-3-")
				svc.Labels["l-custom"] = "l-custom-value"
				return svc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceUpdated Service default/test updated"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Client holds the state so it has to persists the iterations.
			client := fake.NewSimpleClientset(tc.existing...)

			// ApplyService needs to be reentrant so running it the second time should give the same results.
			// (One of the common mistakes is editing the object after computing the hash so it differs the second time.)
			iterations := 2
			if tc.expectedErr != nil {
				iterations = 1
			}
			for i := range iterations {
				t.Run("", func(t *testing.T) {
					ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer ctxCancel()

					recorder := record.NewFakeRecorder(10)

					serviceCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
					svcLister := corev1listers.NewServiceLister(serviceCache)

					if tc.cache != nil {
						for _, obj := range tc.cache {
							err := serviceCache.Add(obj)
							if err != nil {
								t.Fatal(err)
							}
						}
					} else {
						svcList, err := client.CoreV1().Services("").List(ctx, metav1.ListOptions{
							LabelSelector: labels.Everything().String(),
						})
						if err != nil {
							t.Fatal(err)
						}

						for i := range svcList.Items {
							err := serviceCache.Add(&svcList.Items[i])
							if err != nil {
								t.Fatal(err)
							}
						}
					}

					gotSts, gotChanged, gotErr := ApplyService(ctx, client.CoreV1(), svcLister, recorder, tc.required, ApplyOptions{
						ForceOwnership: tc.forceOwnership,
					})
					if !reflect.DeepEqual(gotErr, tc.expectedErr) {
						t.Fatalf("expected %v, got %v", tc.expectedErr, gotErr)
					}

					if !equality.Semantic.DeepEqual(gotSts, tc.expectedService) {
						t.Errorf("expected %#v, got %#v, diff:\n%s", tc.expectedService, gotSts, cmp.Diff(tc.expectedService, gotSts))
					}

					// Make sure such object was actually created.
					if gotSts != nil {
						createdSts, err := client.CoreV1().Services(gotSts.Namespace).Get(ctx, gotSts.Name, metav1.GetOptions{})
						if err != nil {
							t.Error(err)
						}
						if !equality.Semantic.DeepEqual(createdSts, gotSts) {
							t.Errorf("created and returned services differ:\n%s", cmp.Diff(createdSts, gotSts))
						}
					}

					if i == 0 {
						if gotChanged != tc.expectedChanged {
							t.Errorf("expected %t, got %t", tc.expectedChanged, gotChanged)
						}
					} else {
						if gotChanged {
							t.Errorf("object changed in iteration %d", i)
						}
					}

					close(recorder.Events)
					var gotEvents []string
					for e := range recorder.Events {
						gotEvents = append(gotEvents, e)
					}
					if i == 0 {
						if !reflect.DeepEqual(gotEvents, tc.expectedEvents) {
							t.Errorf("expected %v, got %v, diff:\n%s", tc.expectedEvents, gotEvents, cmp.Diff(tc.expectedEvents, gotEvents))
						}
					} else {
						if len(gotEvents) > 0 {
							t.Errorf("unexpected events: %v", gotEvents)
						}
					}
				})
			}
		})
	}
}

func TestApplySecret(t *testing.T) {
	// Using a generating function prevents unwanted mutations.
	newSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test",
				Labels:    map[string]string{},
				OwnerReferences: []metav1.OwnerReference{
					{
						Controller:         pointer.Ptr(true),
						UID:                "abcdefgh",
						APIVersion:         "scylla.scylladb.com/v1",
						Kind:               "ScyllaCluster",
						Name:               "basic",
						BlockOwnerDeletion: pointer.Ptr(true),
					},
				},
			},
			Data: map[string][]byte{},
		}
	}

	newSecretWithHash := func() *corev1.Secret {
		secret := newSecret()
		apimachineryutilruntime.Must(SetHashAnnotation(secret))
		return secret
	}

	tt := []struct {
		name            string
		existing        []runtime.Object
		cache           []runtime.Object // nil cache means autofill from the client
		required        *corev1.Secret
		forceOwnership  bool
		expectedSecret  *corev1.Secret
		expectedChanged bool
		expectedErr     error
		expectedEvents  []string
	}{
		{
			name:            "creates a new secret when there is none",
			existing:        nil,
			required:        newSecret(),
			expectedSecret:  newSecretWithHash(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal SecretCreated Secret default/test created"},
		},
		{
			name: "does nothing if the same secret already exists",
			existing: []runtime.Object{
				newSecretWithHash(),
			},
			required:        newSecret(),
			expectedSecret:  newSecretWithHash(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "does nothing if the same secret already exists and required one has the hash",
			existing: []runtime.Object{
				newSecretWithHash(),
			},
			required:        newSecretWithHash(),
			expectedSecret:  newSecretWithHash(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "updates the secret if it exists without the hash",
			existing: []runtime.Object{
				newSecret(),
			},
			required:        newSecret(),
			expectedSecret:  newSecretWithHash(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal SecretUpdated Secret default/test updated"},
		},
		{
			name:     "fails to create the secret without a controllerRef",
			existing: nil,
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.OwnerReferences = nil
				return secret
			}(),
			expectedSecret:  nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Secret "default/test" is missing controllerRef`),
			expectedEvents:  nil,
		},
		{
			name: "updates the secret if data differs",
			existing: []runtime.Object{
				newSecret(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Data["tls.key"] = []byte("foo")
				return secret
			}(),
			expectedSecret: func() *corev1.Secret {
				secret := newSecret()
				secret.Data["tls.key"] = []byte("foo")
				apimachineryutilruntime.Must(SetHashAnnotation(secret))
				return secret
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal SecretUpdated Secret default/test updated"},
		},
		{
			name: "updates the secret if labels differ",
			existing: []runtime.Object{
				newSecretWithHash(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Labels["foo"] = "bar"
				return secret
			}(),
			expectedSecret: func() *corev1.Secret {
				secret := newSecret()
				secret.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(secret))
				return secret
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal SecretUpdated Secret default/test updated"},
		},
		{
			name: "won't update the secret if an admission changes the sts",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecretWithHash()
					// Simulate admission by changing a value after the hash is computed.
					secret.Data["tls.key"] = []byte("admissionchange")
					return secret
				}(),
			},
			required: newSecret(),
			expectedSecret: func() *corev1.Secret {
				secret := newSecretWithHash()
				// Simulate admission by changing a value after the hash is computed.
				secret.Data["tls.key"] = []byte("admissionchange")
				return secret
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			// We test propagating the RV from required in all the other tests.
			name: "specifying no RV will use the one from the existing object",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecretWithHash()
					secret.ResourceVersion = "21"
					return secret
				}(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.ResourceVersion = ""
				secret.Labels["foo"] = "bar"
				return secret
			}(),
			expectedSecret: func() *corev1.Secret {
				secret := newSecret()
				secret.ResourceVersion = "21"
				secret.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(secret))
				return secret
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal SecretUpdated Secret default/test updated"},
		},
		{
			name:     "update fails if the secret is missing but we still see it in the cache",
			existing: nil,
			cache: []runtime.Object{
				newSecretWithHash(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Labels["foo"] = "bar"
				return secret
			}(),
			expectedSecret:  nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`can't update /v1, Kind=Secret "default/test": %w`, apierrors.NewNotFound(corev1.Resource("secrets"), "test")),
			expectedEvents:  []string{`Warning UpdateSecretFailed Failed to update Secret default/test: secrets "test" not found`},
		},
		{
			name: "update fails if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecret()
					secret.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(secret))
					return secret
				}(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Labels["foo"] = "bar"
				return secret
			}(),
			expectedSecret:  nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Secret "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateSecretFailed Failed to update Secret default/test: /v1, Kind=Secret "default/test" isn't controlled by us`},
		},
		{
			name: "forced update succeeds if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecret()
					secret.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(secret))
					return secret
				}(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Labels["foo"] = "bar"
				return secret
			}(),
			forceOwnership: true,
			expectedSecret: func() *corev1.Secret {
				secret := newSecret()
				secret.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(secret))
				return secret
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal SecretUpdated Secret default/test updated"},
		},
		{
			name: "update succeeds to replace ownerRef kind",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecret()
					secret.OwnerReferences[0].Kind = "WrongKind"
					apimachineryutilruntime.Must(SetHashAnnotation(secret))
					return secret
				}(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				return secret
			}(),
			expectedSecret: func() *corev1.Secret {
				secret := newSecret()
				apimachineryutilruntime.Must(SetHashAnnotation(secret))
				return secret
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal SecretUpdated Secret default/test updated"},
		},
		{
			name: "update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecret()
					secret.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(secret))
					return secret
				}(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Labels["foo"] = "bar"
				return secret
			}(),
			expectedSecret:  nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Secret "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateSecretFailed Failed to update Secret default/test: /v1, Kind=Secret "default/test" isn't controlled by us`},
		},
		{
			name: "forced update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecret()
					secret.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(secret))
					return secret
				}(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Labels["foo"] = "bar"
				return secret
			}(),
			forceOwnership:  true,
			expectedSecret:  nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Secret "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateSecretFailed Failed to update Secret default/test: /v1, Kind=Secret "default/test" isn't controlled by us`},
		},
		{
			name: "all label and annotation keys are kept when the hash matches",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecret()
					secret.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "",
					}
					secret.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(secret))
					secret.Annotations["a-1"] = "a-alpha-changed"
					secret.Annotations["a-3"] = "a-resurrected"
					secret.Annotations["a-custom"] = "custom-value"
					secret.Labels["l-1"] = "l-alpha-changed"
					secret.Labels["l-3"] = "l-resurrected"
					secret.Labels["l-custom"] = "custom-value"
					return secret
				}(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				secret.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				return secret
			}(),
			forceOwnership: false,
			expectedSecret: func() *corev1.Secret {
				secret := newSecret()
				secret.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				secret.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(secret))
				secret.Annotations["a-1"] = "a-alpha-changed"
				secret.Annotations["a-3"] = "a-resurrected"
				secret.Annotations["a-custom"] = "custom-value"
				secret.Labels["l-1"] = "l-alpha-changed"
				secret.Labels["l-3"] = "l-resurrected"
				secret.Labels["l-custom"] = "custom-value"
				return secret
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "only managed label and annotation keys are updated when the hash changes",
			existing: []runtime.Object{
				func() *corev1.Secret {
					secret := newSecret()
					secret.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "a-resurrected",
					}
					secret.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "l-resurrected",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(secret))
					secret.Annotations["a-1"] = "a-alpha-changed"
					secret.Annotations["a-custom"] = "a-custom-value"
					secret.Labels["l-1"] = "l-alpha-changed"
					secret.Labels["l-custom"] = "l-custom-value"
					return secret
				}(),
			},
			required: func() *corev1.Secret {
				secret := newSecret()
				secret.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				secret.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				return secret
			}(),
			forceOwnership: true,
			expectedSecret: func() *corev1.Secret {
				secret := newSecret()
				secret.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				secret.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(secret))
				delete(secret.Annotations, "a-3-")
				secret.Annotations["a-custom"] = "a-custom-value"
				delete(secret.Labels, "l-3-")
				secret.Labels["l-custom"] = "l-custom-value"
				return secret
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal SecretUpdated Secret default/test updated"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Client holds the state so it has to persists the iterations.
			client := fake.NewSimpleClientset(tc.existing...)

			// ApplySecret needs to be reentrant so running it the second time should give the same results.
			// (One of the common mistakes is editing the object after computing the hash so it differs the second time.)
			iterations := 2
			if tc.expectedErr != nil {
				iterations = 1
			}
			for i := range iterations {
				t.Run("", func(t *testing.T) {
					ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer ctxCancel()

					recorder := record.NewFakeRecorder(10)

					secretCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
					secretLister := corev1listers.NewSecretLister(secretCache)

					if tc.cache != nil {
						for _, obj := range tc.cache {
							err := secretCache.Add(obj)
							if err != nil {
								t.Fatal(err)
							}
						}
					} else {
						secretList, err := client.CoreV1().Secrets("").List(ctx, metav1.ListOptions{
							LabelSelector: labels.Everything().String(),
						})
						if err != nil {
							t.Fatal(err)
						}

						for i := range secretList.Items {
							err := secretCache.Add(&secretList.Items[i])
							if err != nil {
								t.Fatal(err)
							}
						}
					}

					gotSts, gotChanged, gotErr := ApplySecret(ctx, client.CoreV1(), secretLister, recorder, tc.required, ApplyOptions{
						ForceOwnership: tc.forceOwnership,
					})
					if !reflect.DeepEqual(gotErr, tc.expectedErr) {
						t.Fatalf("expected %v, got %v", tc.expectedErr, gotErr)
					}

					if !equality.Semantic.DeepEqual(gotSts, tc.expectedSecret) {
						t.Errorf("expected %#v, got %#v, diff:\n%s", tc.expectedSecret, gotSts, cmp.Diff(tc.expectedSecret, gotSts))
					}

					// Make sure such object was actually created.
					if gotSts != nil {
						createdSts, err := client.CoreV1().Secrets(gotSts.Namespace).Get(ctx, gotSts.Name, metav1.GetOptions{})
						if err != nil {
							t.Error(err)
						}
						if !equality.Semantic.DeepEqual(createdSts, gotSts) {
							t.Errorf("created and returned secrets differ:\n%s", cmp.Diff(createdSts, gotSts))
						}
					}

					if i == 0 {
						if gotChanged != tc.expectedChanged {
							t.Errorf("expected %t, got %t", tc.expectedChanged, gotChanged)
						}
					} else {
						if gotChanged {
							t.Errorf("object changed in iteration %d", i)
						}
					}

					close(recorder.Events)
					var gotEvents []string
					for e := range recorder.Events {
						gotEvents = append(gotEvents, e)
					}
					if i == 0 {
						if !reflect.DeepEqual(gotEvents, tc.expectedEvents) {
							t.Errorf("expected %v, got %v, diff:\n%s", tc.expectedEvents, gotEvents, cmp.Diff(tc.expectedEvents, gotEvents))
						}
					} else {
						if len(gotEvents) > 0 {
							t.Errorf("unexpected events: %v", gotEvents)
						}
					}
				})
			}
		})
	}
}

func TestApplyServiceAccount(t *testing.T) {
	// Using a generating function prevents unwanted mutations.
	newSA := func() *corev1.ServiceAccount {
		return &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test",
				// Setting a RV make sure it's propagated to update calls for optimistic concurrency.
				ResourceVersion: "42",
				Labels:          map[string]string{},
			},
		}
	}
	newSAWithControllerRef := func() *corev1.ServiceAccount {
		sa := newSA()
		sa.OwnerReferences = []metav1.OwnerReference{
			{
				Controller:         pointer.Ptr(true),
				UID:                "abcdefgh",
				APIVersion:         "scylla.scylladb.com/v1",
				Kind:               "ScyllaCluster",
				Name:               "basic",
				BlockOwnerDeletion: pointer.Ptr(true),
			},
		}
		return sa
	}

	newSAWithHash := func() *corev1.ServiceAccount {
		sa := newSA()
		apimachineryutilruntime.Must(SetHashAnnotation(sa))
		return sa
	}

	tt := []struct {
		name                      string
		existing                  []runtime.Object
		cache                     []runtime.Object // nil cache means autofill from the client
		forceOwnership            bool
		allowMissingControllerRef bool
		required                  *corev1.ServiceAccount
		expectedSA                *corev1.ServiceAccount
		expectedChanged           bool
		expectedErr               error
		expectedEvents            []string
	}{
		{
			name:                      "creates a new SA when there is none",
			existing:                  nil,
			allowMissingControllerRef: true,
			required:                  newSA(),
			expectedSA:                newSAWithHash(),
			expectedChanged:           true,
			expectedErr:               nil,
			expectedEvents:            []string{"Normal ServiceAccountCreated ServiceAccount default/test created"},
		},
		{
			name: "does nothing if the same SA already exists",
			existing: []runtime.Object{
				newSAWithHash(),
			},
			allowMissingControllerRef: true,
			required:                  newSA(),
			expectedSA:                newSAWithHash(),
			expectedChanged:           false,
			expectedErr:               nil,
			expectedEvents:            nil,
		},
		{
			name: "does nothing if the same SA already exists and required one has the hash",
			existing: []runtime.Object{
				newSAWithHash(),
			},
			allowMissingControllerRef: true,
			required:                  newSAWithHash(),
			expectedSA:                newSAWithHash(),
			expectedChanged:           false,
			expectedErr:               nil,
			expectedEvents:            nil,
		},
		{
			name: "updates the SA if it exists without the hash",
			existing: []runtime.Object{
				newSA(),
			},
			allowMissingControllerRef: true,
			required:                  newSA(),
			expectedSA:                newSAWithHash(),
			expectedChanged:           true,
			expectedErr:               nil,
			expectedEvents:            []string{"Normal ServiceAccountUpdated ServiceAccount default/test updated"},
		},
		{
			name:                      "fails to create the SA without a controllerRef",
			existing:                  nil,
			allowMissingControllerRef: false,
			required: func() *corev1.ServiceAccount {
				sa := newSA()
				sa.OwnerReferences = nil
				return sa
			}(),
			expectedSA:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=ServiceAccount "default/test" is missing controllerRef`),
			expectedEvents:  nil,
		},
		{
			name: "updates the SA when AutomountServiceAccountToken differ",
			existing: []runtime.Object{
				newSA(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.ServiceAccount {
				sa := newSA()
				sa.AutomountServiceAccountToken = pointer.Ptr(true)
				return sa
			}(),
			expectedSA: func() *corev1.ServiceAccount {
				sa := newSA()
				sa.AutomountServiceAccountToken = pointer.Ptr(true)
				apimachineryutilruntime.Must(SetHashAnnotation(sa))
				return sa
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceAccountUpdated ServiceAccount default/test updated"},
		},
		{
			name: "updates the SA if labels differ",
			existing: []runtime.Object{
				newSAWithHash(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.ServiceAccount {
				sa := newSA()
				sa.Labels["foo"] = "bar"
				return sa
			}(),
			expectedSA: func() *corev1.ServiceAccount {
				sa := newSA()
				sa.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(sa))
				return sa
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceAccountUpdated ServiceAccount default/test updated"},
		},
		{
			name: "won't update the SA if an admission changes the crb",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					sa := newSAWithHash()
					// Simulate admission by changing a value after the hash is computed.
					sa.AutomountServiceAccountToken = pointer.Ptr(true)
					return sa
				}(),
			},
			allowMissingControllerRef: true,
			required:                  newSA(),
			expectedSA: func() *corev1.ServiceAccount {
				sa := newSAWithHash()
				// Simulate admission by changing a value after the hash is computed.
				sa.AutomountServiceAccountToken = pointer.Ptr(true)
				return sa
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			// We test propagating the RV from required in all the other test.
			name: "specifying no RV will use the one from the existing object",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					crb := newSAWithHash()
					crb.ResourceVersion = "21"
					return crb
				}(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.ServiceAccount {
				sa := newSA()
				sa.ResourceVersion = ""
				sa.AutomountServiceAccountToken = pointer.Ptr(true)
				return sa
			}(),
			expectedSA: func() *corev1.ServiceAccount {
				sa := newSA()
				sa.ResourceVersion = "21"
				sa.AutomountServiceAccountToken = pointer.Ptr(true)
				apimachineryutilruntime.Must(SetHashAnnotation(sa))
				return sa
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceAccountUpdated ServiceAccount default/test updated"},
		},
		{
			name:     "update fails if the SA is missing but we still see it in the cache",
			existing: nil,
			cache: []runtime.Object{
				newSAWithHash(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.ServiceAccount {
				sa := newSA()
				sa.AutomountServiceAccountToken = pointer.Ptr(true)
				return sa
			}(),
			expectedSA:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`can't update /v1, Kind=ServiceAccount "default/test": %w`, apierrors.NewNotFound(corev1.Resource("serviceaccounts"), "test")),
			expectedEvents:  []string{`Warning UpdateServiceAccountFailed Failed to update ServiceAccount default/test: serviceaccounts "test" not found`},
		},
		{
			name: "update fails if the existing object has ownerRef and required hasn't",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					sa := newSAWithControllerRef()
					apimachineryutilruntime.Must(SetHashAnnotation(sa))
					return sa
				}(),
			},
			allowMissingControllerRef: true,
			required:                  newSA(),
			expectedSA:                nil,
			expectedChanged:           false,
			expectedErr:               fmt.Errorf(`/v1, Kind=ServiceAccount "default/test" isn't controlled by us`),
			expectedEvents:            []string{`Warning UpdateServiceAccountFailed Failed to update ServiceAccount default/test: /v1, Kind=ServiceAccount "default/test" isn't controlled by us`},
		},
		{
			name: "forced update succeeds if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					sa := newSA()
					apimachineryutilruntime.Must(SetHashAnnotation(sa))
					return sa
				}(),
			},
			required: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				sa.Labels["foo"] = "bar"
				return sa
			}(),
			forceOwnership: true,
			expectedSA: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				sa.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(sa))
				return sa
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceAccountUpdated ServiceAccount default/test updated"},
		},
		{
			name: "update succeeds to replace ownerRef kind",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					sa := newSAWithControllerRef()
					sa.OwnerReferences[0].Kind = "WrongKind"
					apimachineryutilruntime.Must(SetHashAnnotation(sa))
					return sa
				}(),
			},
			required: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				return sa
			}(),
			expectedSA: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				apimachineryutilruntime.Must(SetHashAnnotation(sa))
				return sa
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceAccountUpdated ServiceAccount default/test updated"},
		},
		{
			name: "update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					sa := newSAWithControllerRef()
					sa.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(sa))
					return sa
				}(),
			},
			required: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				sa.Labels["foo"] = "bar"
				return sa
			}(),
			expectedSA:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=ServiceAccount "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateServiceAccountFailed Failed to update ServiceAccount default/test: /v1, Kind=ServiceAccount "default/test" isn't controlled by us`},
		},
		{
			name: "forced update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					sa := newSAWithControllerRef()
					sa.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(sa))
					return sa
				}(),
			},
			required: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				sa.Labels["foo"] = "bar"
				return sa
			}(),
			forceOwnership:  true,
			expectedSA:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=ServiceAccount "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateServiceAccountFailed Failed to update ServiceAccount default/test: /v1, Kind=ServiceAccount "default/test" isn't controlled by us`},
		},
		{
			name: "all label and annotation keys are kept when the hash matches",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					sa := newSAWithControllerRef()
					sa.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "",
					}
					sa.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(sa))
					sa.Annotations["a-1"] = "a-alpha-changed"
					sa.Annotations["a-3"] = "a-resurrected"
					sa.Annotations["a-custom"] = "custom-value"
					sa.Labels["l-1"] = "l-alpha-changed"
					sa.Labels["l-3"] = "l-resurrected"
					sa.Labels["l-custom"] = "custom-value"
					return sa
				}(),
			},
			required: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				sa.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				sa.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				return sa
			}(),
			forceOwnership: false,
			expectedSA: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				sa.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				sa.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(sa))
				sa.Annotations["a-1"] = "a-alpha-changed"
				sa.Annotations["a-3"] = "a-resurrected"
				sa.Annotations["a-custom"] = "custom-value"
				sa.Labels["l-1"] = "l-alpha-changed"
				sa.Labels["l-3"] = "l-resurrected"
				sa.Labels["l-custom"] = "custom-value"
				return sa
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "only managed label and annotation keys are updated when the hash changes",
			existing: []runtime.Object{
				func() *corev1.ServiceAccount {
					sa := newSAWithControllerRef()
					sa.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "a-resurrected",
					}
					sa.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "l-resurrected",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(sa))
					sa.Annotations["a-1"] = "a-alpha-changed"
					sa.Annotations["a-custom"] = "a-custom-value"
					sa.Labels["l-1"] = "l-alpha-changed"
					sa.Labels["l-custom"] = "l-custom-value"
					return sa
				}(),
			},
			required: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				sa.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				sa.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				return sa
			}(),
			forceOwnership: true,
			expectedSA: func() *corev1.ServiceAccount {
				sa := newSAWithControllerRef()
				sa.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				sa.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(sa))
				delete(sa.Annotations, "a-3-")
				sa.Annotations["a-custom"] = "a-custom-value"
				delete(sa.Labels, "l-3-")
				sa.Labels["l-custom"] = "l-custom-value"
				return sa
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ServiceAccountUpdated ServiceAccount default/test updated"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Client holds the state, so it has to persist the iterations.
			client := fake.NewSimpleClientset(tc.existing...)

			// ApplyClusterRole needs to be reentrant so running it the second time should give the same results.
			// (One of the common mistakes is editing the object after computing the hash, so it differs the second time.)
			iterations := 2
			if tc.expectedErr != nil {
				iterations = 1
			}
			for i := range iterations {
				t.Run("", func(t *testing.T) {
					ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer ctxCancel()

					recorder := record.NewFakeRecorder(10)

					saCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
					crbLister := corev1listers.NewServiceAccountLister(saCache)

					if tc.cache != nil {
						for _, obj := range tc.cache {
							err := saCache.Add(obj)
							if err != nil {
								t.Fatal(err)
							}
						}
					} else {
						crList, err := client.CoreV1().ServiceAccounts(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
							LabelSelector: labels.Everything().String(),
						})
						if err != nil {
							t.Fatal(err)
						}

						for i := range crList.Items {
							err := saCache.Add(&crList.Items[i])
							if err != nil {
								t.Fatal(err)
							}
						}
					}

					gotSA, gotChanged, gotErr := ApplyServiceAccount(ctx, client.CoreV1(), crbLister, recorder, tc.required, ApplyOptions{
						ForceOwnership:            tc.forceOwnership,
						AllowMissingControllerRef: tc.allowMissingControllerRef,
					})
					if !reflect.DeepEqual(gotErr, tc.expectedErr) {
						t.Fatalf("expected %v, got %v", tc.expectedErr, gotErr)
					}

					if !equality.Semantic.DeepEqual(gotSA, tc.expectedSA) {
						t.Errorf("expected and got SA differ:\n%s", cmp.Diff(tc.expectedSA, gotSA))
					}

					// Make sure such object was actually created.
					if gotSA != nil {
						createdSA, err := client.CoreV1().ServiceAccounts(gotSA.Namespace).Get(ctx, gotSA.Name, metav1.GetOptions{})
						if err != nil {
							t.Error(err)
						}
						if !equality.Semantic.DeepEqual(createdSA, gotSA) {
							t.Errorf("created and returned ServiceAccounts differ:\n%s", cmp.Diff(createdSA, gotSA))
						}
					}

					if i == 0 {
						if gotChanged != tc.expectedChanged {
							t.Errorf("expected %t, got %t", tc.expectedChanged, gotChanged)
						}
					} else {
						if gotChanged {
							t.Errorf("object changed in iteration %d", i)
						}
					}

					close(recorder.Events)
					var gotEvents []string
					for e := range recorder.Events {
						gotEvents = append(gotEvents, e)
					}
					if i == 0 {
						if !reflect.DeepEqual(gotEvents, tc.expectedEvents) {
							t.Errorf("expected %v, got %v, diff:\n%s", tc.expectedEvents, gotEvents, cmp.Diff(tc.expectedEvents, gotEvents))
						}
					} else {
						if len(gotEvents) > 0 {
							t.Errorf("unexpected events: %v", gotEvents)
						}
					}
				})
			}
		})
	}
}

func TestApplyConfigMap(t *testing.T) {
	// Using a generating function prevents unwanted mutations.
	newConfigMap := func() *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test",
				Labels:    map[string]string{},
				OwnerReferences: []metav1.OwnerReference{
					{
						Controller:         pointer.Ptr(true),
						UID:                "abcdefgh",
						APIVersion:         "scylla.scylladb.com/v1",
						Kind:               "ScyllaCluster",
						Name:               "basic",
						BlockOwnerDeletion: pointer.Ptr(true),
					},
				},
			},
			Data: map[string]string{},
		}
	}

	newConfigMapWithHash := func() *corev1.ConfigMap {
		cm := newConfigMap()
		apimachineryutilruntime.Must(SetHashAnnotation(cm))
		return cm
	}

	tt := []struct {
		name            string
		existing        []runtime.Object
		cache           []runtime.Object // nil cache means autofill from the client
		required        *corev1.ConfigMap
		expectedCM      *corev1.ConfigMap
		expectedChanged bool
		expectedErr     error
		expectedEvents  []string
	}{
		{
			name:            "creates a new configmap when there is none",
			existing:        nil,
			required:        newConfigMap(),
			expectedCM:      newConfigMapWithHash(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ConfigMapCreated ConfigMap default/test created"},
		},
		{
			name: "does nothing if the same configmap already exists",
			existing: []runtime.Object{
				newConfigMapWithHash(),
			},
			required:        newConfigMap(),
			expectedCM:      newConfigMapWithHash(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "does nothing if the same configmap already exists and required one has the hash",
			existing: []runtime.Object{
				newConfigMapWithHash(),
			},
			required:        newConfigMapWithHash(),
			expectedCM:      newConfigMapWithHash(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "updates the configmap if it exists without the hash",
			existing: []runtime.Object{
				newConfigMap(),
			},
			required:        newConfigMap(),
			expectedCM:      newConfigMapWithHash(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ConfigMapUpdated ConfigMap default/test updated"},
		},
		{
			name:     "fails to create the configmap without a controllerRef",
			existing: nil,
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.OwnerReferences = nil
				return cm
			}(),
			expectedCM:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=ConfigMap "default/test" is missing controllerRef`),
			expectedEvents:  nil,
		},
		{
			name: "updates the configmap if data differs",
			existing: []runtime.Object{
				newConfigMap(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Data["tls.key"] = "foo"
				return cm
			}(),
			expectedCM: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Data["tls.key"] = "foo"
				apimachineryutilruntime.Must(SetHashAnnotation(cm))
				return cm
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ConfigMapUpdated ConfigMap default/test updated"},
		},
		{
			name: "updates the configmap if labels differ",
			existing: []runtime.Object{
				newConfigMapWithHash(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Labels["foo"] = "bar"
				return cm
			}(),
			expectedCM: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(cm))
				return cm
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ConfigMapUpdated ConfigMap default/test updated"},
		},
		{
			name: "won't update the configmap if an admission changes the sts",
			existing: []runtime.Object{
				func() *corev1.ConfigMap {
					cm := newConfigMapWithHash()
					// Simulate admission by changing a value after the hash is computed.
					cm.Data["tls.key"] = "admissionchange"
					return cm
				}(),
			},
			required: newConfigMap(),
			expectedCM: func() *corev1.ConfigMap {
				cm := newConfigMapWithHash()
				// Simulate admission by changing a value after the hash is computed.
				cm.Data["tls.key"] = "admissionchange"
				return cm
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			// We test propagating the RV from required in all the other tests.
			name: "specifying no RV will use the one from the existing object",
			existing: []runtime.Object{
				func() *corev1.ConfigMap {
					cm := newConfigMapWithHash()
					cm.ResourceVersion = "21"
					return cm
				}(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.ResourceVersion = ""
				cm.Labels["foo"] = "bar"
				return cm
			}(),
			expectedCM: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.ResourceVersion = "21"
				cm.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(cm))
				return cm
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ConfigMapUpdated ConfigMap default/test updated"},
		},
		{
			name:     "update fails if the configmap is missing but we still see it in the cache",
			existing: nil,
			cache: []runtime.Object{
				newConfigMapWithHash(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Labels["foo"] = "bar"
				return cm
			}(),
			expectedCM:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`can't update /v1, Kind=ConfigMap "default/test": %w`, apierrors.NewNotFound(corev1.Resource("configmaps"), "test")),
			expectedEvents:  []string{`Warning UpdateConfigMapFailed Failed to update ConfigMap default/test: configmaps "test" not found`},
		},
		{
			name: "update fails if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.ConfigMap {
					cm := newConfigMap()
					cm.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(cm))
					return cm
				}(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Labels["foo"] = "bar"
				return cm
			}(),
			expectedCM:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=ConfigMap "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateConfigMapFailed Failed to update ConfigMap default/test: /v1, Kind=ConfigMap "default/test" isn't controlled by us`},
		},
		{
			name: "update succeeds to replace ownerRef kind",
			existing: []runtime.Object{
				func() *corev1.ConfigMap {
					cm := newConfigMap()
					cm.OwnerReferences[0].Kind = "WrongKind"
					apimachineryutilruntime.Must(SetHashAnnotation(cm))
					return cm
				}(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				return cm
			}(),
			expectedCM: func() *corev1.ConfigMap {
				cm := newConfigMap()
				apimachineryutilruntime.Must(SetHashAnnotation(cm))
				return cm
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ConfigMapUpdated ConfigMap default/test updated"},
		},
		{
			name: "update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.ConfigMap {
					cm := newConfigMap()
					cm.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(cm))
					return cm
				}(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Labels["foo"] = "bar"
				return cm
			}(),
			expectedCM:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=ConfigMap "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdateConfigMapFailed Failed to update ConfigMap default/test: /v1, Kind=ConfigMap "default/test" isn't controlled by us`},
		},
		{
			name: "all label and annotation keys are kept when the hash matches",
			existing: []runtime.Object{
				func() *corev1.ConfigMap {
					cm := newConfigMap()
					cm.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "",
					}
					cm.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(cm))
					cm.Annotations["a-1"] = "a-alpha-changed"
					cm.Annotations["a-3"] = "a-resurrected"
					cm.Annotations["a-custom"] = "custom-value"
					cm.Labels["l-1"] = "l-alpha-changed"
					cm.Labels["l-3"] = "l-resurrected"
					cm.Labels["l-custom"] = "custom-value"
					return cm
				}(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				cm.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				return cm
			}(),
			expectedCM: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				cm.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(cm))
				cm.Annotations["a-1"] = "a-alpha-changed"
				cm.Annotations["a-3"] = "a-resurrected"
				cm.Annotations["a-custom"] = "custom-value"
				cm.Labels["l-1"] = "l-alpha-changed"
				cm.Labels["l-3"] = "l-resurrected"
				cm.Labels["l-custom"] = "custom-value"
				return cm
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "only managed label and annotation keys are updated when the hash changes",
			existing: []runtime.Object{
				func() *corev1.ConfigMap {
					cm := newConfigMap()
					cm.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "a-resurrected",
					}
					cm.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "l-resurrected",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(cm))
					cm.Annotations["a-1"] = "a-alpha-changed"
					cm.Annotations["a-custom"] = "a-custom-value"
					cm.Labels["l-1"] = "l-alpha-changed"
					cm.Labels["l-custom"] = "l-custom-value"
					return cm
				}(),
			},
			required: func() *corev1.ConfigMap {
				cm := newConfigMap()
				cm.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				cm.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				return cm
			}(),
			expectedCM: func() *corev1.ConfigMap {
				configMap := newConfigMap()
				configMap.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				configMap.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(configMap))
				delete(configMap.Annotations, "a-3-")
				configMap.Annotations["a-custom"] = "a-custom-value"
				delete(configMap.Labels, "l-3-")
				configMap.Labels["l-custom"] = "l-custom-value"
				return configMap
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal ConfigMapUpdated ConfigMap default/test updated"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Client holds the state so it has to persists the iterations.
			client := fake.NewSimpleClientset(tc.existing...)

			// ApplyConfigMap needs to be reentrant so running it the second time should give the same results.
			// (One of the common mistakes is editing the object after computing the hash so it differs the second time.)
			iterations := 2
			if tc.expectedErr != nil {
				iterations = 1
			}
			for i := range iterations {
				t.Run("", func(t *testing.T) {
					ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer ctxCancel()

					recorder := record.NewFakeRecorder(10)

					configmapCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
					configmapLister := corev1listers.NewConfigMapLister(configmapCache)

					if tc.cache != nil {
						for _, obj := range tc.cache {
							err := configmapCache.Add(obj)
							if err != nil {
								t.Fatal(err)
							}
						}
					} else {
						configmapList, err := client.CoreV1().ConfigMaps("").List(ctx, metav1.ListOptions{
							LabelSelector: labels.Everything().String(),
						})
						if err != nil {
							t.Fatal(err)
						}

						for i := range configmapList.Items {
							err := configmapCache.Add(&configmapList.Items[i])
							if err != nil {
								t.Fatal(err)
							}
						}
					}

					gotSts, gotChanged, gotErr := ApplyConfigMap(ctx, client.CoreV1(), configmapLister, recorder, tc.required, ApplyOptions{})
					if !reflect.DeepEqual(gotErr, tc.expectedErr) {
						t.Fatalf("expected %v, got %v", tc.expectedErr, gotErr)
					}

					if !equality.Semantic.DeepEqual(gotSts, tc.expectedCM) {
						t.Errorf("expected %#v, got %#v, diff:\n%s", tc.expectedCM, gotSts, cmp.Diff(tc.expectedCM, gotSts))
					}

					// Make sure such object was actually created.
					if gotSts != nil {
						createdSts, err := client.CoreV1().ConfigMaps(gotSts.Namespace).Get(ctx, gotSts.Name, metav1.GetOptions{})
						if err != nil {
							t.Error(err)
						}
						if !equality.Semantic.DeepEqual(createdSts, gotSts) {
							t.Errorf("created and returned configmaps differ:\n%s", cmp.Diff(createdSts, gotSts))
						}
					}

					if i == 0 {
						if gotChanged != tc.expectedChanged {
							t.Errorf("expected %t, got %t", tc.expectedChanged, gotChanged)
						}
					} else {
						if gotChanged {
							t.Errorf("object changed in iteration %d", i)
						}
					}

					close(recorder.Events)
					var gotEvents []string
					for e := range recorder.Events {
						gotEvents = append(gotEvents, e)
					}
					if i == 0 {
						if !reflect.DeepEqual(gotEvents, tc.expectedEvents) {
							t.Errorf("expected %v, got %v, diff:\n%s", tc.expectedEvents, gotEvents, cmp.Diff(tc.expectedEvents, gotEvents))
						}
					} else {
						if len(gotEvents) > 0 {
							t.Errorf("unexpected events: %v", gotEvents)
						}
					}
				})
			}
		})
	}
}

func TestApplyNamespace(t *testing.T) {
	// Using a generating function prevents unwanted mutations.
	newNS := func() *corev1.Namespace {
		return &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test",
				// Setting a RV make sure it's propagated to update calls for optimistic concurrency.
				ResourceVersion: "42",
				Labels:          map[string]string{},
			},
		}
	}

	newNSWithHash := func() *corev1.Namespace {
		ns := newNS()
		apimachineryutilruntime.Must(SetHashAnnotation(ns))
		return ns
	}

	tt := []struct {
		name                      string
		existing                  []runtime.Object
		cache                     []runtime.Object // nil cache means autofill from the client
		allowMissingControllerRef bool
		required                  *corev1.Namespace
		expectedNS                *corev1.Namespace
		expectedChanged           bool
		expectedErr               error
		expectedEvents            []string
	}{
		{
			name:                      "creates a new Namespace when there is none",
			existing:                  nil,
			allowMissingControllerRef: true,
			required:                  newNS(),
			expectedNS:                newNSWithHash(),
			expectedChanged:           true,
			expectedErr:               nil,
			expectedEvents:            []string{"Normal NamespaceCreated Namespace test created"},
		},
		{
			name: "does nothing if the same Namespace already exists",
			existing: []runtime.Object{
				newNSWithHash(),
			},
			allowMissingControllerRef: true,
			required:                  newNS(),
			expectedNS:                newNSWithHash(),
			expectedChanged:           false,
			expectedErr:               nil,
			expectedEvents:            nil,
		},
		{
			name: "does nothing if the same Namespace already exists and required one has the hash",
			existing: []runtime.Object{
				newNSWithHash(),
			},
			allowMissingControllerRef: true,
			required:                  newNSWithHash(),
			expectedNS:                newNSWithHash(),
			expectedChanged:           false,
			expectedErr:               nil,
			expectedEvents:            nil,
		},
		{
			name: "updates the Namespace if it exists without the hash",
			existing: []runtime.Object{
				newNS(),
			},
			allowMissingControllerRef: true,
			required:                  newNS(),
			expectedNS:                newNSWithHash(),
			expectedChanged:           true,
			expectedErr:               nil,
			expectedEvents:            []string{"Normal NamespaceUpdated Namespace test updated"},
		},
		{
			name:                      "fails to create the Namespace without a controllerRef",
			existing:                  nil,
			allowMissingControllerRef: false,
			required: func() *corev1.Namespace {
				ns := newNS()
				ns.OwnerReferences = nil
				return ns
			}(),
			expectedNS:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Namespace "test" is missing controllerRef`),
			expectedEvents:  nil,
		},
		{
			name: "updates the Namespace when Finalizers differ",
			existing: []runtime.Object{
				newNS(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.Namespace {
				ns := newNS()
				ns.Finalizers = []string{"boop"}
				return ns
			}(),
			expectedNS: func() *corev1.Namespace {
				ns := newNS()
				ns.Finalizers = []string{"boop"}
				apimachineryutilruntime.Must(SetHashAnnotation(ns))
				return ns
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal NamespaceUpdated Namespace test updated"},
		},
		{
			name: "updates the Namespace if labels differ",
			existing: []runtime.Object{
				newNSWithHash(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.Namespace {
				ns := newNS()
				ns.Labels["foo"] = "bar"
				return ns
			}(),
			expectedNS: func() *corev1.Namespace {
				ns := newNS()
				ns.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(ns))
				return ns
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal NamespaceUpdated Namespace test updated"},
		},
		{
			name: "won't update the Namespace if an admission changes the ns",
			existing: []runtime.Object{
				func() *corev1.Namespace {
					ns := newNSWithHash()
					// Simulate admission by changing a value after the hash is computed.
					ns.Finalizers = []string{"boop"}
					return ns
				}(),
			},
			allowMissingControllerRef: true,
			required:                  newNS(),
			expectedNS: func() *corev1.Namespace {
				ns := newNSWithHash()
				// Simulate admission by changing a value after the hash is computed.
				ns.Finalizers = []string{"boop"}
				return ns
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			// We test propagating the RV from required in all the other test.
			name: "specifying no RV will use the one from the existing object",
			existing: []runtime.Object{
				func() *corev1.Namespace {
					ns := newNSWithHash()
					ns.ResourceVersion = "21"
					return ns
				}(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.Namespace {
				ns := newNS()
				ns.ResourceVersion = ""
				ns.Finalizers = []string{"boop"}
				return ns
			}(),
			expectedNS: func() *corev1.Namespace {
				ns := newNS()
				ns.ResourceVersion = "21"
				ns.Finalizers = []string{"boop"}
				apimachineryutilruntime.Must(SetHashAnnotation(ns))
				return ns
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal NamespaceUpdated Namespace test updated"},
		},
		{
			name:     "update fails if the Namespace is missing but we still see it in the cache",
			existing: nil,
			cache: []runtime.Object{
				newNSWithHash(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.Namespace {
				ns := newNS()
				ns.Finalizers = []string{"boop"}
				return ns
			}(),
			expectedNS:      nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`can't update /v1, Kind=Namespace "test": %w`, apierrors.NewNotFound(corev1.Resource("namespaces"), "test")),
			expectedEvents:  []string{`Warning UpdateNamespaceFailed Failed to update Namespace test: namespaces "test" not found`},
		},
		{
			name: "update fails if the existing object has ownerRef and required hasn't",
			existing: []runtime.Object{
				func() *corev1.Namespace {
					ns := newNS()
					ns.OwnerReferences = []metav1.OwnerReference{
						{
							Controller:         pointer.Ptr(true),
							UID:                "abcdefgh",
							APIVersion:         "scylla.scylladb.com/v1",
							Kind:               "ScyllaCluster",
							Name:               "basic",
							BlockOwnerDeletion: pointer.Ptr(true),
						},
					}
					apimachineryutilruntime.Must(SetHashAnnotation(ns))
					return ns
				}(),
			},
			allowMissingControllerRef: true,
			required:                  newNS(),
			expectedNS:                nil,
			expectedChanged:           false,
			expectedErr:               fmt.Errorf(`/v1, Kind=Namespace "test" isn't controlled by us`),
			expectedEvents:            []string{`Warning UpdateNamespaceFailed Failed to update Namespace test: /v1, Kind=Namespace "test" isn't controlled by us`},
		},
		{
			name: "all label and annotation keys are kept when the hash matches",
			existing: []runtime.Object{
				func() *corev1.Namespace {
					ns := newNS()
					ns.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "",
					}
					ns.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(ns))
					ns.Annotations["a-1"] = "a-alpha-changed"
					ns.Annotations["a-3"] = "a-resurrected"
					ns.Annotations["a-custom"] = "custom-value"
					ns.Labels["l-1"] = "l-alpha-changed"
					ns.Labels["l-3"] = "l-resurrected"
					ns.Labels["l-custom"] = "custom-value"
					return ns
				}(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.Namespace {
				ns := newNS()
				ns.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				ns.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				return ns
			}(),
			expectedNS: func() *corev1.Namespace {
				ns := newNS()
				ns.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				ns.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(ns))
				ns.Annotations["a-1"] = "a-alpha-changed"
				ns.Annotations["a-3"] = "a-resurrected"
				ns.Annotations["a-custom"] = "custom-value"
				ns.Labels["l-1"] = "l-alpha-changed"
				ns.Labels["l-3"] = "l-resurrected"
				ns.Labels["l-custom"] = "custom-value"
				return ns
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "only managed label and annotation keys are updated when the hash changes",
			existing: []runtime.Object{
				func() *corev1.Namespace {
					ns := newNS()
					ns.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "a-resurrected",
					}
					ns.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "l-resurrected",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(ns))
					ns.Annotations["a-1"] = "a-alpha-changed"
					ns.Annotations["a-custom"] = "a-custom-value"
					ns.Labels["l-1"] = "l-alpha-changed"
					ns.Labels["l-custom"] = "l-custom-value"
					return ns
				}(),
			},
			allowMissingControllerRef: true,
			required: func() *corev1.Namespace {
				ns := newNS()
				ns.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				ns.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				return ns
			}(),
			expectedNS: func() *corev1.Namespace {
				ns := newNS()
				ns.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				ns.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(ns))
				delete(ns.Annotations, "a-3-")
				ns.Annotations["a-custom"] = "a-custom-value"
				delete(ns.Labels, "l-3-")
				ns.Labels["l-custom"] = "l-custom-value"
				return ns
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal NamespaceUpdated Namespace test updated"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Client holds the state so it has to persicr the iterations.
			client := fake.NewSimpleClientset(tc.existing...)

			// ApplyClusterRole needs to be reentrant so running it the second time should give the same results.
			// (One of the common mistakes is editing the object after computing the hash so it differs the second time.)
			iterations := 2
			if tc.expectedErr != nil {
				iterations = 1
			}
			for i := range iterations {
				t.Run("", func(t *testing.T) {
					ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer ctxCancel()

					recorder := record.NewFakeRecorder(10)

					nsCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
					nsLister := corev1listers.NewNamespaceLister(nsCache)

					if tc.cache != nil {
						for _, obj := range tc.cache {
							err := nsCache.Add(obj)
							if err != nil {
								t.Fatal(err)
							}
						}
					} else {
						nsList, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
							LabelSelector: labels.Everything().String(),
						})
						if err != nil {
							t.Fatal(err)
						}

						for i := range nsList.Items {
							err := nsCache.Add(&nsList.Items[i])
							if err != nil {
								t.Fatal(err)
							}
						}
					}

					gotNS, gotChanged, gotErr := ApplyNamespace(ctx, client.CoreV1(), nsLister, recorder, tc.required, ApplyOptions{
						AllowMissingControllerRef: tc.allowMissingControllerRef,
					})
					if !reflect.DeepEqual(gotErr, tc.expectedErr) {
						t.Fatalf("expected %v, got %v", tc.expectedErr, gotErr)
					}

					if !equality.Semantic.DeepEqual(gotNS, tc.expectedNS) {
						t.Errorf("expected %#v, got %#v, diff:\n%s", tc.expectedNS, gotNS, cmp.Diff(tc.expectedNS, gotNS))
					}

					// Make sure such object was actually created.
					if gotNS != nil {
						createdNS, err := client.CoreV1().Namespaces().Get(ctx, gotNS.Name, metav1.GetOptions{})
						if err != nil {
							t.Error(err)
						}
						if !equality.Semantic.DeepEqual(createdNS, gotNS) {
							t.Errorf("created and returned Namespaces differ:\n%s", cmp.Diff(createdNS, gotNS))
						}
					}

					if i == 0 {
						if gotChanged != tc.expectedChanged {
							t.Errorf("expected %t, got %t", tc.expectedChanged, gotChanged)
						}
					} else {
						if gotChanged {
							t.Errorf("object changed in iteration %d", i)
						}
					}

					close(recorder.Events)
					var gotEvents []string
					for e := range recorder.Events {
						gotEvents = append(gotEvents, e)
					}
					if i == 0 {
						if !reflect.DeepEqual(gotEvents, tc.expectedEvents) {
							t.Errorf("expected %v, got %v, diff:\n%s", tc.expectedEvents, gotEvents, cmp.Diff(tc.expectedEvents, gotEvents))
						}
					} else {
						if len(gotEvents) > 0 {
							t.Errorf("unexpected events: %v", gotEvents)
						}
					}
				})
			}
		})
	}
}

func TestApplyEndpoints(t *testing.T) {
	// Using a generating function prevents unwanted mutations.
	newEndpoints := func() *corev1.Endpoints {
		return &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test",
				Labels:    map[string]string{},
				OwnerReferences: []metav1.OwnerReference{
					{
						Controller:         pointer.Ptr(true),
						UID:                "abcdefgh",
						APIVersion:         "scylla.scylladb.com/v1",
						Kind:               "ScyllaCluster",
						Name:               "basic",
						BlockOwnerDeletion: pointer.Ptr(true),
					},
				},
			},
			Subsets: []corev1.EndpointSubset{
				{
					Addresses: []corev1.EndpointAddress{
						{
							IP:       "1.1.1.1",
							Hostname: "abc",
							NodeName: pointer.Ptr("node"),
						},
					},
					Ports: []corev1.EndpointPort{
						{
							Name:     "port",
							Port:     1337,
							Protocol: corev1.ProtocolTCP,
						},
					},
				},
			},
		}
	}

	newEndpointsWithHash := func() *corev1.Endpoints {
		Endpoints := newEndpoints()
		apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
		return Endpoints
	}

	tt := []struct {
		name              string
		existing          []runtime.Object
		cache             []runtime.Object // nil cache means autofill from the client
		required          *corev1.Endpoints
		expectedEndpoints *corev1.Endpoints
		expectedChanged   bool
		expectedErr       error
		expectedEvents    []string
	}{
		{
			name:              "creates a new Endpoints when there is none",
			existing:          nil,
			required:          newEndpoints(),
			expectedEndpoints: newEndpointsWithHash(),
			expectedChanged:   true,
			expectedErr:       nil,
			expectedEvents:    []string{"Normal EndpointsCreated Endpoints default/test created"},
		},
		{
			name: "does nothing if the same Endpoints already exists",
			existing: []runtime.Object{
				newEndpointsWithHash(),
			},
			required:          newEndpoints(),
			expectedEndpoints: newEndpointsWithHash(),
			expectedChanged:   false,
			expectedErr:       nil,
			expectedEvents:    nil,
		},
		{
			name: "does nothing if the same Endpoints already exists and required one has the hash",
			existing: []runtime.Object{
				newEndpointsWithHash(),
			},
			required:          newEndpointsWithHash(),
			expectedEndpoints: newEndpointsWithHash(),
			expectedChanged:   false,
			expectedErr:       nil,
			expectedEvents:    nil,
		},
		{
			name: "updates the Endpoints if it exists without the hash",
			existing: []runtime.Object{
				newEndpoints(),
			},
			required:          newEndpoints(),
			expectedEndpoints: newEndpointsWithHash(),
			expectedChanged:   true,
			expectedErr:       nil,
			expectedEvents:    []string{"Normal EndpointsUpdated Endpoints default/test updated"},
		},
		{
			name:     "fails to create the Endpoints without a controllerRef",
			existing: nil,
			required: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.OwnerReferences = nil
				return Endpoints
			}(),
			expectedEndpoints: nil,
			expectedChanged:   false,
			expectedErr:       fmt.Errorf(`/v1, Kind=Endpoints "default/test" is missing controllerRef`),
			expectedEvents:    nil,
		},
		{
			name: "updates the Endpoints when it differs",
			existing: []runtime.Object{
				newEndpoints(),
			},
			required: func() *corev1.Endpoints {
				endpoints := newEndpoints()
				endpoints.Subsets[0].Addresses = append(endpoints.Subsets[0].Addresses, corev1.EndpointAddress{
					IP: "2.2.2.2",
				})
				return endpoints
			}(),
			expectedEndpoints: func() *corev1.Endpoints {
				endpoints := newEndpoints()
				endpoints.Subsets[0].Addresses = append(endpoints.Subsets[0].Addresses, corev1.EndpointAddress{
					IP: "2.2.2.2",
				})
				apimachineryutilruntime.Must(SetHashAnnotation(endpoints))
				return endpoints
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal EndpointsUpdated Endpoints default/test updated"},
		},
		{
			name: "updates the Endpoints if labels differ",
			existing: []runtime.Object{
				newEndpointsWithHash(),
			},
			required: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Labels["foo"] = "bar"
				return Endpoints
			}(),
			expectedEndpoints: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
				return Endpoints
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal EndpointsUpdated Endpoints default/test updated"},
		},
		{
			name: "won't update the Endpoints if an admission changes the sts",
			existing: []runtime.Object{
				func() *corev1.Endpoints {
					endpoints := newEndpointsWithHash()
					// Simulate admission by changing a value after the hash is computed.
					endpoints.Subsets[0].Addresses = append(endpoints.Subsets[0].Addresses, corev1.EndpointAddress{
						IP: "2.2.2.2",
					})
					return endpoints
				}(),
			},
			required: newEndpoints(),
			expectedEndpoints: func() *corev1.Endpoints {
				endpoints := newEndpointsWithHash()
				// Simulate admission by changing a value after the hash is computed.
				endpoints.Subsets[0].Addresses = append(endpoints.Subsets[0].Addresses, corev1.EndpointAddress{
					IP: "2.2.2.2",
				})
				return endpoints
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			// We test propagating the RV from required in all the other tests.
			name: "specifying no RV will use the one from the existing object",
			existing: []runtime.Object{
				func() *corev1.Endpoints {
					Endpoints := newEndpointsWithHash()
					Endpoints.ResourceVersion = "21"
					return Endpoints
				}(),
			},
			required: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.ResourceVersion = ""
				Endpoints.Labels["foo"] = "bar"
				return Endpoints
			}(),
			expectedEndpoints: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.ResourceVersion = "21"
				Endpoints.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
				return Endpoints
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal EndpointsUpdated Endpoints default/test updated"},
		},
		{
			name:     "update fails if the Endpoints is missing but we still see it in the cache",
			existing: nil,
			cache: []runtime.Object{
				newEndpointsWithHash(),
			},
			required: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Labels["foo"] = "bar"
				return Endpoints
			}(),
			expectedEndpoints: nil,
			expectedChanged:   false,
			expectedErr:       fmt.Errorf(`can't update /v1, Kind=Endpoints "default/test": %w`, apierrors.NewNotFound(corev1.Resource("endpoints"), "test")),
			expectedEvents:    []string{`Warning UpdateEndpointsFailed Failed to update Endpoints default/test: endpoints "test" not found`},
		},
		{
			name: "update fails if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.Endpoints {
					Endpoints := newEndpoints()
					Endpoints.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
					return Endpoints
				}(),
			},
			required: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Labels["foo"] = "bar"
				return Endpoints
			}(),
			expectedEndpoints: nil,
			expectedChanged:   false,
			expectedErr:       fmt.Errorf(`/v1, Kind=Endpoints "default/test" isn't controlled by us`),
			expectedEvents:    []string{`Warning UpdateEndpointsFailed Failed to update Endpoints default/test: /v1, Kind=Endpoints "default/test" isn't controlled by us`},
		},
		{
			name: "update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.Endpoints {
					Endpoints := newEndpoints()
					Endpoints.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
					return Endpoints
				}(),
			},
			required: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Labels["foo"] = "bar"
				return Endpoints
			}(),
			expectedEndpoints: nil,
			expectedChanged:   false,
			expectedErr:       fmt.Errorf(`/v1, Kind=Endpoints "default/test" isn't controlled by us`),
			expectedEvents:    []string{`Warning UpdateEndpointsFailed Failed to update Endpoints default/test: /v1, Kind=Endpoints "default/test" isn't controlled by us`},
		},
		{
			name: "all label and annotation keys are kept when the hash matches",
			existing: []runtime.Object{
				func() *corev1.Endpoints {
					Endpoints := newEndpoints()
					Endpoints.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "",
					}
					Endpoints.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
					Endpoints.Annotations["a-1"] = "a-alpha-changed"
					Endpoints.Annotations["a-3"] = "a-resurrected"
					Endpoints.Annotations["a-custom"] = "custom-value"
					Endpoints.Labels["l-1"] = "l-alpha-changed"
					Endpoints.Labels["l-3"] = "l-resurrected"
					Endpoints.Labels["l-custom"] = "custom-value"
					return Endpoints
				}(),
			},
			required: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				Endpoints.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				return Endpoints
			}(),
			expectedEndpoints: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				Endpoints.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
				Endpoints.Annotations["a-1"] = "a-alpha-changed"
				Endpoints.Annotations["a-3"] = "a-resurrected"
				Endpoints.Annotations["a-custom"] = "custom-value"
				Endpoints.Labels["l-1"] = "l-alpha-changed"
				Endpoints.Labels["l-3"] = "l-resurrected"
				Endpoints.Labels["l-custom"] = "custom-value"
				return Endpoints
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "only managed label and annotation keys are updated when the hash changes",
			existing: []runtime.Object{
				func() *corev1.Endpoints {
					Endpoints := newEndpoints()
					Endpoints.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "a-resurrected",
					}
					Endpoints.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "l-resurrected",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
					Endpoints.Annotations["a-1"] = "a-alpha-changed"
					Endpoints.Annotations["a-custom"] = "a-custom-value"
					Endpoints.Labels["l-1"] = "l-alpha-changed"
					Endpoints.Labels["l-custom"] = "l-custom-value"
					return Endpoints
				}(),
			},
			required: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				Endpoints.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				return Endpoints
			}(),
			expectedEndpoints: func() *corev1.Endpoints {
				Endpoints := newEndpoints()
				Endpoints.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				Endpoints.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(Endpoints))
				delete(Endpoints.Annotations, "a-3-")
				Endpoints.Annotations["a-custom"] = "a-custom-value"
				delete(Endpoints.Labels, "l-3-")
				Endpoints.Labels["l-custom"] = "l-custom-value"
				return Endpoints
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal EndpointsUpdated Endpoints default/test updated"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Client holds the state so it has to persist the iterations.
			client := fake.NewSimpleClientset(tc.existing...)

			// ApplyEndpoints needs to be reentrant so running it the second time should give the same results.
			// (One of the common mistakes is editing the object after computing the hash so it differs the second time.)
			iterations := 2
			if tc.expectedErr != nil {
				iterations = 1
			}
			for i := range iterations {
				t.Run("", func(t *testing.T) {
					ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer ctxCancel()

					recorder := record.NewFakeRecorder(10)

					endpointsCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
					endpointsLister := corev1listers.NewEndpointsLister(endpointsCache)

					if tc.cache != nil {
						for _, obj := range tc.cache {
							err := endpointsCache.Add(obj)
							if err != nil {
								t.Fatal(err)
							}
						}
					} else {
						endpointsList, err := client.CoreV1().Endpoints("").List(ctx, metav1.ListOptions{
							LabelSelector: labels.Everything().String(),
						})
						if err != nil {
							t.Fatal(err)
						}

						for i := range endpointsList.Items {
							err := endpointsCache.Add(&endpointsList.Items[i])
							if err != nil {
								t.Fatal(err)
							}
						}
					}

					gotObj, gotChanged, gotErr := ApplyEndpoints(ctx, client.CoreV1(), endpointsLister, recorder, tc.required, ApplyOptions{})
					if !reflect.DeepEqual(gotErr, tc.expectedErr) {
						t.Fatalf("expected %v, got %v", tc.expectedErr, gotErr)
					}

					if !equality.Semantic.DeepEqual(gotObj, tc.expectedEndpoints) {
						t.Errorf("expected %#v, got %#v, diff:\n%s", tc.expectedEndpoints, gotObj, cmp.Diff(tc.expectedEndpoints, gotObj))
					}

					// Make sure such object was actually created.
					if gotObj != nil {
						created, err := client.CoreV1().Endpoints(gotObj.Namespace).Get(ctx, gotObj.Name, metav1.GetOptions{})
						if err != nil {
							t.Error(err)
						}
						if !equality.Semantic.DeepEqual(created, gotObj) {
							t.Errorf("created and returned Endpointss differ:\n%s", cmp.Diff(created, gotObj))
						}
					}

					if i == 0 {
						if gotChanged != tc.expectedChanged {
							t.Errorf("expected %t, got %t", tc.expectedChanged, gotChanged)
						}
					} else {
						if gotChanged {
							t.Errorf("object changed in iteration %d", i)
						}
					}

					close(recorder.Events)
					var gotEvents []string
					for e := range recorder.Events {
						gotEvents = append(gotEvents, e)
					}
					if i == 0 {
						if !reflect.DeepEqual(gotEvents, tc.expectedEvents) {
							t.Errorf("expected %v, got %v, diff:\n%s", tc.expectedEvents, gotEvents, cmp.Diff(tc.expectedEvents, gotEvents))
						}
					} else {
						if len(gotEvents) > 0 {
							t.Errorf("unexpected events: %v", gotEvents)
						}
					}
				})
			}
		})
	}
}

func TestApplyPod(t *testing.T) {
	// Using a generating function prevents unwanted mutations.
	newPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test",
				Labels:    map[string]string{},
				OwnerReferences: []metav1.OwnerReference{
					{
						Controller:         pointer.Ptr(true),
						UID:                "abcdefgh",
						APIVersion:         "scylla.scylladb.com/v1",
						Kind:               "ScyllaCluster",
						Name:               "basic",
						BlockOwnerDeletion: pointer.Ptr(true),
					},
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test",
						Image: "docker.io/scylladb/scylla-operator:latest",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("50Mi"),
							},
						},
					},
				},
				NodeName: "test",
			},
		}
	}

	newPodWithHash := func() *corev1.Pod {
		pod := newPod()
		apimachineryutilruntime.Must(SetHashAnnotation(pod))
		return pod
	}

	tt := []struct {
		name            string
		existing        []runtime.Object
		cache           []runtime.Object // nil cache means autofill from the client
		required        *corev1.Pod
		forceOwnership  bool
		expectedPod     *corev1.Pod
		expectedChanged bool
		expectedErr     error
		expectedEvents  []string
	}{
		{
			name:            "creates a new pod when there is none",
			existing:        nil,
			required:        newPod(),
			expectedPod:     newPodWithHash(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PodCreated Pod default/test created"},
		},
		{
			name: "does nothing if the same pod already exists",
			existing: []runtime.Object{
				newPodWithHash(),
			},
			required:        newPod(),
			expectedPod:     newPodWithHash(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "does nothing if the same pod already exists and required one has the hash",
			existing: []runtime.Object{
				newPodWithHash(),
			},
			required:        newPodWithHash(),
			expectedPod:     newPodWithHash(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "updates the pod if it exists without the hash",
			existing: []runtime.Object{
				newPod(),
			},
			required:        newPod(),
			expectedPod:     newPodWithHash(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PodUpdated Pod default/test updated"},
		},
		{
			name:     "fails to create the pod without a controllerRef",
			existing: nil,
			required: func() *corev1.Pod {
				pod := newPod()
				pod.OwnerReferences = nil
				return pod
			}(),
			expectedPod:     nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Pod "default/test" is missing controllerRef`),
			expectedEvents:  nil,
		},
		{
			name: "updates the pod if resources differ",
			existing: []runtime.Object{
				newPod(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("20m")
				return pod
			}(),
			expectedPod: func() *corev1.Pod {
				pod := newPod()
				pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("20m")
				apimachineryutilruntime.Must(SetHashAnnotation(pod))
				return pod
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PodUpdated Pod default/test updated"},
		},
		{
			name: "updates the pod if labels differ",
			existing: []runtime.Object{
				newPodWithHash(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Labels["foo"] = "bar"
				return pod
			}(),
			expectedPod: func() *corev1.Pod {
				pod := newPod()
				pod.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(pod))
				return pod
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PodUpdated Pod default/test updated"},
		},
		{
			name: "won't update the pod if an admission changes it",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPodWithHash()
					// Simulate admission by changing a value after the hash is computed.
					pod.Finalizers = append(pod.Finalizers, "admissionfinalizer")
					return pod
				}(),
			},
			required: newPod(),
			expectedPod: func() *corev1.Pod {
				pod := newPodWithHash()
				// Simulate admission by changing a value after the hash is computed.
				pod.Finalizers = append(pod.Finalizers, "admissionfinalizer")
				return pod
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			// We test propagating the RV from required in all the other tests.
			name: "specifying no RV will use the one from the existing object",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPodWithHash()
					pod.ResourceVersion = "21"
					return pod
				}(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.ResourceVersion = ""
				pod.Labels["foo"] = "bar"
				return pod
			}(),
			expectedPod: func() *corev1.Pod {
				pod := newPod()
				pod.ResourceVersion = "21"
				pod.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(pod))
				return pod
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PodUpdated Pod default/test updated"},
		},
		{
			name:     "update fails if the pod is missing but we still see it in the cache",
			existing: nil,
			cache: []runtime.Object{
				newPodWithHash(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Labels["foo"] = "bar"
				return pod
			}(),
			expectedPod:     nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`can't update /v1, Kind=Pod "default/test": %w`, apierrors.NewNotFound(corev1.Resource("pods"), "test")),
			expectedEvents:  []string{`Warning UpdatePodFailed Failed to update Pod default/test: pods "test" not found`},
		},
		{
			name: "update fails if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod()
					pod.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(pod))
					return pod
				}(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Labels["foo"] = "bar"
				return pod
			}(),
			expectedPod:     nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Pod "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdatePodFailed Failed to update Pod default/test: /v1, Kind=Pod "default/test" isn't controlled by us`},
		},
		{
			name: "forced update succeeds if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod()
					pod.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(pod))
					return pod
				}(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Labels["foo"] = "bar"
				return pod
			}(),
			forceOwnership: true,
			expectedPod: func() *corev1.Pod {
				pod := newPod()
				pod.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(pod))
				return pod
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PodUpdated Pod default/test updated"},
		},
		{
			name: "update succeeds to replace ownerRef kind",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod()
					pod.OwnerReferences[0].Kind = "WrongKind"
					apimachineryutilruntime.Must(SetHashAnnotation(pod))
					return pod
				}(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				return pod
			}(),
			expectedPod: func() *corev1.Pod {
				pod := newPod()
				apimachineryutilruntime.Must(SetHashAnnotation(pod))
				return pod
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PodUpdated Pod default/test updated"},
		},
		{
			name: "update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod()
					pod.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(pod))
					return pod
				}(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Labels["foo"] = "bar"
				return pod
			}(),
			expectedPod:     nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Pod "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdatePodFailed Failed to update Pod default/test: /v1, Kind=Pod "default/test" isn't controlled by us`},
		},
		{
			name: "forced update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod()
					pod.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(pod))
					return pod
				}(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Labels["foo"] = "bar"
				return pod
			}(),
			forceOwnership:  true,
			expectedPod:     nil,
			expectedChanged: false,
			expectedErr:     fmt.Errorf(`/v1, Kind=Pod "default/test" isn't controlled by us`),
			expectedEvents:  []string{`Warning UpdatePodFailed Failed to update Pod default/test: /v1, Kind=Pod "default/test" isn't controlled by us`},
		},
		{
			name: "all label and annotation keys are kept when the hash matches",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod()
					pod.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "",
					}
					pod.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(pod))
					pod.Annotations["a-1"] = "a-alpha-changed"
					pod.Annotations["a-3"] = "a-resurrected"
					pod.Annotations["a-custom"] = "custom-value"
					pod.Labels["l-1"] = "l-alpha-changed"
					pod.Labels["l-3"] = "l-resurrected"
					pod.Labels["l-custom"] = "custom-value"
					return pod
				}(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				pod.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				return pod
			}(),
			forceOwnership: false,
			expectedPod: func() *corev1.Pod {
				pod := newPod()
				pod.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				pod.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(pod))
				pod.Annotations["a-1"] = "a-alpha-changed"
				pod.Annotations["a-3"] = "a-resurrected"
				pod.Annotations["a-custom"] = "custom-value"
				pod.Labels["l-1"] = "l-alpha-changed"
				pod.Labels["l-3"] = "l-resurrected"
				pod.Labels["l-custom"] = "custom-value"
				return pod
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "only managed label and annotation keys are updated when the hash changes",
			existing: []runtime.Object{
				func() *corev1.Pod {
					pod := newPod()
					pod.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "a-resurrected",
					}
					pod.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "l-resurrected",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(pod))
					pod.Annotations["a-1"] = "a-alpha-changed"
					pod.Annotations["a-custom"] = "a-custom-value"
					pod.Labels["l-1"] = "l-alpha-changed"
					pod.Labels["l-custom"] = "l-custom-value"
					return pod
				}(),
			},
			required: func() *corev1.Pod {
				pod := newPod()
				pod.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				pod.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				return pod
			}(),
			forceOwnership: true,
			expectedPod: func() *corev1.Pod {
				pod := newPod()
				pod.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				pod.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(pod))
				delete(pod.Annotations, "a-3-")
				pod.Annotations["a-custom"] = "a-custom-value"
				delete(pod.Labels, "l-3-")
				pod.Labels["l-custom"] = "l-custom-value"
				return pod
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PodUpdated Pod default/test updated"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Client holds the state so it has to persists the iterations.
			client := fake.NewSimpleClientset(tc.existing...)

			// ApplyPod needs to be reentrant so running it the second time should give the same results.
			// (One of the common mistakes is editing the object after computing the hash so it differs the second time.)
			iterations := 2
			if tc.expectedErr != nil {
				iterations = 1
			}
			for i := range iterations {
				t.Run("", func(t *testing.T) {
					ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer ctxCancel()

					recorder := record.NewFakeRecorder(10)

					podCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
					podLister := corev1listers.NewPodLister(podCache)

					if tc.cache != nil {
						for _, obj := range tc.cache {
							err := podCache.Add(obj)
							if err != nil {
								t.Fatal(err)
							}
						}
					} else {
						podList, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
							LabelSelector: labels.Everything().String(),
						})
						if err != nil {
							t.Fatal(err)
						}

						for i := range podList.Items {
							err := podCache.Add(&podList.Items[i])
							if err != nil {
								t.Fatal(err)
							}
						}
					}

					gotPod, gotChanged, gotErr := ApplyPod(ctx, client.CoreV1(), podLister, recorder, tc.required, ApplyOptions{
						ForceOwnership: tc.forceOwnership,
					})
					if !reflect.DeepEqual(gotErr, tc.expectedErr) {
						t.Fatalf("expected %v, got %v", tc.expectedErr, gotErr)
					}

					if !equality.Semantic.DeepEqual(gotPod, tc.expectedPod) {
						t.Errorf("expected %#v, got %#v, diff:\n%s", tc.expectedPod, gotPod, cmp.Diff(tc.expectedPod, gotPod))
					}

					// Make sure such object was actually created.
					if gotPod != nil {
						createdPod, err := client.CoreV1().Pods(gotPod.Namespace).Get(ctx, gotPod.Name, metav1.GetOptions{})
						if err != nil {
							t.Error(err)
						}
						if !equality.Semantic.DeepEqual(createdPod, gotPod) {
							t.Errorf("created and returned pods differ:\n%s", cmp.Diff(createdPod, gotPod))
						}
					}

					if i == 0 {
						if gotChanged != tc.expectedChanged {
							t.Errorf("expected %t, got %t", tc.expectedChanged, gotChanged)
						}
					} else {
						if gotChanged {
							t.Errorf("object changed in iteration %d", i)
						}
					}

					close(recorder.Events)
					var gotEvents []string
					for e := range recorder.Events {
						gotEvents = append(gotEvents, e)
					}
					if i == 0 {
						if !reflect.DeepEqual(gotEvents, tc.expectedEvents) {
							t.Errorf("expected %v, got %v, diff:\n%s", tc.expectedEvents, gotEvents, cmp.Diff(tc.expectedEvents, gotEvents))
						}
					} else {
						if len(gotEvents) > 0 {
							t.Errorf("unexpected events: %v", gotEvents)
						}
					}
				})
			}
		})
	}
}

func TestApplyPersistentVolumeClaim(t *testing.T) {
	// Using a generating function prevents unwanted mutations.
	newPersistentVolumeClaim := func() *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test",
				Labels:    map[string]string{},
				OwnerReferences: []metav1.OwnerReference{
					{
						Controller:         pointer.Ptr(true),
						UID:                "abcdefgh",
						APIVersion:         "scylla.scylladb.com/v1",
						Kind:               "ScyllaCluster",
						Name:               "basic",
						BlockOwnerDeletion: pointer.Ptr(true),
					},
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
			},
		}
	}

	newPersistentVolumeClaimWithHash := func() *corev1.PersistentVolumeClaim {
		pvc := newPersistentVolumeClaim()
		apimachineryutilruntime.Must(SetHashAnnotation(pvc))
		return pvc
	}

	tt := []struct {
		name                          string
		existing                      []runtime.Object
		cache                         []runtime.Object // nil cache means autofill from the client
		required                      *corev1.PersistentVolumeClaim
		forceOwnership                bool
		expectedPersistentVolumeClaim *corev1.PersistentVolumeClaim
		expectedChanged               bool
		expectedErr                   error
		expectedEvents                []string
	}{
		{
			name:                          "creates a new pvc when there is none",
			existing:                      nil,
			required:                      newPersistentVolumeClaim(),
			expectedPersistentVolumeClaim: newPersistentVolumeClaimWithHash(),
			expectedChanged:               true,
			expectedErr:                   nil,
			expectedEvents:                []string{"Normal PersistentVolumeClaimCreated PersistentVolumeClaim default/test created"},
		},
		{
			name: "does nothing if the same pvc already exists",
			existing: []runtime.Object{
				newPersistentVolumeClaimWithHash(),
			},
			required:                      newPersistentVolumeClaim(),
			expectedPersistentVolumeClaim: newPersistentVolumeClaimWithHash(),
			expectedChanged:               false,
			expectedErr:                   nil,
			expectedEvents:                nil,
		},
		{
			name: "does nothing if the same pvc already exists and required one has the hash",
			existing: []runtime.Object{
				newPersistentVolumeClaimWithHash(),
			},
			required:                      newPersistentVolumeClaimWithHash(),
			expectedPersistentVolumeClaim: newPersistentVolumeClaimWithHash(),
			expectedChanged:               false,
			expectedErr:                   nil,
			expectedEvents:                nil,
		},
		{
			name: "updates the pvc if it exists without the hash",
			existing: []runtime.Object{
				newPersistentVolumeClaim(),
			},
			required:                      newPersistentVolumeClaim(),
			expectedPersistentVolumeClaim: newPersistentVolumeClaimWithHash(),
			expectedChanged:               true,
			expectedErr:                   nil,
			expectedEvents:                []string{"Normal PersistentVolumeClaimUpdated PersistentVolumeClaim default/test updated"},
		},
		{
			name:     "fails to create the pvc without a controllerRef",
			existing: nil,
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.OwnerReferences = nil
				return pvc
			}(),
			expectedPersistentVolumeClaim: nil,
			expectedChanged:               false,
			expectedErr:                   fmt.Errorf(`/v1, Kind=PersistentVolumeClaim "default/test" is missing controllerRef`),
			expectedEvents:                nil,
		},
		{
			name: "updates the pvc if access mode differs",
			existing: []runtime.Object{
				newPersistentVolumeClaim(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
				}
				return pvc
			}(),
			expectedPersistentVolumeClaim: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteMany,
				}
				apimachineryutilruntime.Must(SetHashAnnotation(pvc))
				return pvc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PersistentVolumeClaimUpdated PersistentVolumeClaim default/test updated"},
		},
		{
			name: "updates the pvc if labels differ",
			existing: []runtime.Object{
				newPersistentVolumeClaimWithHash(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Labels["foo"] = "bar"
				return pvc
			}(),
			expectedPersistentVolumeClaim: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(pvc))
				return pvc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PersistentVolumeClaimUpdated PersistentVolumeClaim default/test updated"},
		},
		{
			name: "won't update the pvc if an admission changes it",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaimWithHash()
					// Simulate admission by changing a value after the hash is computed.
					pvc.Finalizers = append(pvc.Finalizers, "admissionfinalizer")
					return pvc
				}(),
			},
			required: newPersistentVolumeClaim(),
			expectedPersistentVolumeClaim: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaimWithHash()
				// Simulate admission by changing a value after the hash is computed.
				pvc.Finalizers = append(pvc.Finalizers, "admissionfinalizer")
				return pvc
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			// We test propagating the RV from required in all the other tests.
			name: "specifying no RV will use the one from the existing object",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaimWithHash()
					pvc.ResourceVersion = "21"
					return pvc
				}(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.ResourceVersion = ""
				pvc.Labels["foo"] = "bar"
				return pvc
			}(),
			expectedPersistentVolumeClaim: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.ResourceVersion = "21"
				pvc.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(pvc))
				return pvc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PersistentVolumeClaimUpdated PersistentVolumeClaim default/test updated"},
		},
		{
			name:     "update fails if the pvc is missing but we still see it in the cache",
			existing: nil,
			cache: []runtime.Object{
				newPersistentVolumeClaimWithHash(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Labels["foo"] = "bar"
				return pvc
			}(),
			expectedPersistentVolumeClaim: nil,
			expectedChanged:               false,
			expectedErr:                   fmt.Errorf(`can't update /v1, Kind=PersistentVolumeClaim "default/test": %w`, apierrors.NewNotFound(corev1.Resource("persistentvolumeclaims"), "test")),
			expectedEvents:                []string{`Warning UpdatePersistentVolumeClaimFailed Failed to update PersistentVolumeClaim default/test: persistentvolumeclaims "test" not found`},
		},
		{
			name: "update fails if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaim()
					pvc.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(pvc))
					return pvc
				}(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Labels["foo"] = "bar"
				return pvc
			}(),
			expectedPersistentVolumeClaim: nil,
			expectedChanged:               false,
			expectedErr:                   fmt.Errorf(`/v1, Kind=PersistentVolumeClaim "default/test" isn't controlled by us`),
			expectedEvents:                []string{`Warning UpdatePersistentVolumeClaimFailed Failed to update PersistentVolumeClaim default/test: /v1, Kind=PersistentVolumeClaim "default/test" isn't controlled by us`},
		},
		{
			name: "forced update succeeds if the existing object has no ownerRef",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaim()
					pvc.OwnerReferences = nil
					apimachineryutilruntime.Must(SetHashAnnotation(pvc))
					return pvc
				}(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Labels["foo"] = "bar"
				return pvc
			}(),
			forceOwnership: true,
			expectedPersistentVolumeClaim: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Labels["foo"] = "bar"
				apimachineryutilruntime.Must(SetHashAnnotation(pvc))
				return pvc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PersistentVolumeClaimUpdated PersistentVolumeClaim default/test updated"},
		},
		{
			name: "update succeeds to replace ownerRef kind",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaim()
					pvc.OwnerReferences[0].Kind = "WrongKind"
					apimachineryutilruntime.Must(SetHashAnnotation(pvc))
					return pvc
				}(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				return pvc
			}(),
			expectedPersistentVolumeClaim: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				apimachineryutilruntime.Must(SetHashAnnotation(pvc))
				return pvc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PersistentVolumeClaimUpdated PersistentVolumeClaim default/test updated"},
		},
		{
			name: "update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaim()
					pvc.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(pvc))
					return pvc
				}(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Labels["foo"] = "bar"
				return pvc
			}(),
			expectedPersistentVolumeClaim: nil,
			expectedChanged:               false,
			expectedErr:                   fmt.Errorf(`/v1, Kind=PersistentVolumeClaim "default/test" isn't controlled by us`),
			expectedEvents:                []string{`Warning UpdatePersistentVolumeClaimFailed Failed to update PersistentVolumeClaim default/test: /v1, Kind=PersistentVolumeClaim "default/test" isn't controlled by us`},
		},
		{
			name: "forced update fails if the existing object is owned by someone else",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaim()
					pvc.OwnerReferences[0].UID = "42"
					apimachineryutilruntime.Must(SetHashAnnotation(pvc))
					return pvc
				}(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Labels["foo"] = "bar"
				return pvc
			}(),
			forceOwnership:                true,
			expectedPersistentVolumeClaim: nil,
			expectedChanged:               false,
			expectedErr:                   fmt.Errorf(`/v1, Kind=PersistentVolumeClaim "default/test" isn't controlled by us`),
			expectedEvents:                []string{`Warning UpdatePersistentVolumeClaimFailed Failed to update PersistentVolumeClaim default/test: /v1, Kind=PersistentVolumeClaim "default/test" isn't controlled by us`},
		},
		{
			name: "all label and annotation keys are kept when the hash matches",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaim()
					pvc.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "",
					}
					pvc.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(pvc))
					pvc.Annotations["a-1"] = "a-alpha-changed"
					pvc.Annotations["a-3"] = "a-resurrected"
					pvc.Annotations["a-custom"] = "custom-value"
					pvc.Labels["l-1"] = "l-alpha-changed"
					pvc.Labels["l-3"] = "l-resurrected"
					pvc.Labels["l-custom"] = "custom-value"
					return pvc
				}(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				pvc.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				return pvc
			}(),
			forceOwnership: false,
			expectedPersistentVolumeClaim: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Annotations = map[string]string{
					"a-1":  "a-alpha",
					"a-2":  "a-beta",
					"a-3-": "",
				}
				pvc.Labels = map[string]string{
					"l-1":  "l-alpha",
					"l-2":  "l-beta",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(pvc))
				pvc.Annotations["a-1"] = "a-alpha-changed"
				pvc.Annotations["a-3"] = "a-resurrected"
				pvc.Annotations["a-custom"] = "custom-value"
				pvc.Labels["l-1"] = "l-alpha-changed"
				pvc.Labels["l-3"] = "l-resurrected"
				pvc.Labels["l-custom"] = "custom-value"
				return pvc
			}(),
			expectedChanged: false,
			expectedErr:     nil,
			expectedEvents:  nil,
		},
		{
			name: "only managed label and annotation keys are updated when the hash changes",
			existing: []runtime.Object{
				func() *corev1.PersistentVolumeClaim {
					pvc := newPersistentVolumeClaim()
					pvc.Annotations = map[string]string{
						"a-1":  "a-alpha",
						"a-2":  "a-beta",
						"a-3-": "a-resurrected",
					}
					pvc.Labels = map[string]string{
						"l-1":  "l-alpha",
						"l-2":  "l-beta",
						"l-3-": "l-resurrected",
					}
					apimachineryutilruntime.Must(SetHashAnnotation(pvc))
					pvc.Annotations["a-1"] = "a-alpha-changed"
					pvc.Annotations["a-custom"] = "a-custom-value"
					pvc.Labels["l-1"] = "l-alpha-changed"
					pvc.Labels["l-custom"] = "l-custom-value"
					return pvc
				}(),
			},
			required: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				pvc.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				return pvc
			}(),
			forceOwnership: true,
			expectedPersistentVolumeClaim: func() *corev1.PersistentVolumeClaim {
				pvc := newPersistentVolumeClaim()
				pvc.Annotations = map[string]string{
					"a-1":  "a-alpha-x",
					"a-2":  "a-beta-x",
					"a-3-": "",
				}
				pvc.Labels = map[string]string{
					"l-1":  "l-alpha-x",
					"l-2":  "l-beta-x",
					"l-3-": "",
				}
				apimachineryutilruntime.Must(SetHashAnnotation(pvc))
				delete(pvc.Annotations, "a-3-")
				pvc.Annotations["a-custom"] = "a-custom-value"
				delete(pvc.Labels, "l-3-")
				pvc.Labels["l-custom"] = "l-custom-value"
				return pvc
			}(),
			expectedChanged: true,
			expectedErr:     nil,
			expectedEvents:  []string{"Normal PersistentVolumeClaimUpdated PersistentVolumeClaim default/test updated"},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Client holds the state so it has to persists the iterations.
			client := fake.NewSimpleClientset(tc.existing...)

			// ApplyPersistentVolumeClaim needs to be reentrant so running it the second time should give the same results.
			// (One of the common mistakes is editing the object after computing the hash so it differs the second time.)
			iterations := 2
			if tc.expectedErr != nil {
				iterations = 1
			}
			for i := range iterations {
				t.Run("", func(t *testing.T) {
					ctx, ctxCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer ctxCancel()

					recorder := record.NewFakeRecorder(10)

					pvcCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
					pvcLister := corev1listers.NewPersistentVolumeClaimLister(pvcCache)

					if tc.cache != nil {
						for _, obj := range tc.cache {
							err := pvcCache.Add(obj)
							if err != nil {
								t.Fatal(err)
							}
						}
					} else {
						pvcList, err := client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{
							LabelSelector: labels.Everything().String(),
						})
						if err != nil {
							t.Fatal(err)
						}

						for i := range pvcList.Items {
							err := pvcCache.Add(&pvcList.Items[i])
							if err != nil {
								t.Fatal(err)
							}
						}
					}

					gotPersistentVolumeClaim, gotChanged, gotErr := ApplyPersistentVolumeClaim(ctx, client.CoreV1(), pvcLister, recorder, tc.required, ApplyOptions{
						ForceOwnership: tc.forceOwnership,
					})
					if !reflect.DeepEqual(gotErr, tc.expectedErr) {
						t.Fatalf("expected %v, got %v", tc.expectedErr, gotErr)
					}

					if !equality.Semantic.DeepEqual(gotPersistentVolumeClaim, tc.expectedPersistentVolumeClaim) {
						t.Errorf("expected %#v, got %#v, diff:\n%s", tc.expectedPersistentVolumeClaim, gotPersistentVolumeClaim, cmp.Diff(tc.expectedPersistentVolumeClaim, gotPersistentVolumeClaim))
					}

					// Make sure such object was actually created.
					if gotPersistentVolumeClaim != nil {
						createdPersistentVolumeClaim, err := client.CoreV1().PersistentVolumeClaims(gotPersistentVolumeClaim.Namespace).Get(ctx, gotPersistentVolumeClaim.Name, metav1.GetOptions{})
						if err != nil {
							t.Error(err)
						}
						if !equality.Semantic.DeepEqual(createdPersistentVolumeClaim, gotPersistentVolumeClaim) {
							t.Errorf("created and returned pvcs differ:\n%s", cmp.Diff(createdPersistentVolumeClaim, gotPersistentVolumeClaim))
						}
					}

					if i == 0 {
						if gotChanged != tc.expectedChanged {
							t.Errorf("expected %t, got %t", tc.expectedChanged, gotChanged)
						}
					} else {
						if gotChanged {
							t.Errorf("object changed in iteration %d", i)
						}
					}

					close(recorder.Events)
					var gotEvents []string
					for e := range recorder.Events {
						gotEvents = append(gotEvents, e)
					}
					if i == 0 {
						if !reflect.DeepEqual(gotEvents, tc.expectedEvents) {
							t.Errorf("expected %v, got %v, diff:\n%s", tc.expectedEvents, gotEvents, cmp.Diff(tc.expectedEvents, gotEvents))
						}
					} else {
						if len(gotEvents) > 0 {
							t.Errorf("unexpected events: %v", gotEvents)
						}
					}
				})
			}
		})
	}
}
