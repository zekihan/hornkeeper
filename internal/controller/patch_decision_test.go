package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A claim update between the decision and the patch must drop that patch.
// The attempt still requeues, so the new claim is applied on the next pass.
func TestClaimResourceVersionChangeSkipsPatch(t *testing.T) {
	f := newFixture(t)
	bumped := false
	f.r.Reader = interceptor.NewClient(f.c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if bumped || obj.GetObjectKind().GroupVersionKind() != BackupTargetGVK {
			return nil
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(f.pvc), pvc); err != nil {
			return err
		}
		if pvc.Annotations == nil {
			pvc.Annotations = map[string]string{}
		}
		pvc.Annotations["unrelated"] = "touch"
		bumped = true
		return c.Update(ctx, pvc)
	}})
	result, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pvc)})
	if err != nil {
		t.Fatal(err)
	}
	if !bumped {
		t.Fatal("backup target lookup did not update the claim")
	}
	if result.RequeueAfter != f.r.Config.ResyncInterval {
		t.Fatal("skipped patch was not requeued")
	}
	f.settings(t, "default", 2)
	if f.patches != 1 {
		t.Fatalf("patches = %d, want only the replica update", f.patches)
	}
	if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues("backupTargetName", "waiting")); got != 1 {
		t.Fatalf("backup target waiting count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues("numberOfReplicas", "patched")); got != 1 {
		t.Fatalf("replica patched count = %v, want 1", got)
	}
}

// The pre-patch recheck runs again after a conflict. A claim that disappeared
// must not be patched on the retry.
func TestRetryStopsWhenClaimDisappears(t *testing.T) {
	f := newFixture(t)
	attempts := 0
	f.r.Client = interceptor.NewClient(f.c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		attempts++
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: f.pvc.Namespace, Name: f.pvc.Name}}
		if err := c.Delete(ctx, claim); err != nil {
			return err
		}
		return apierrors.NewConflict(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, obj.GetName(), errors.New("conflict"))
	}})
	result, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pvc)})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("patch attempts = %d, want 1", attempts)
	}
	if result.RequeueAfter != f.r.Config.ResyncInterval {
		t.Fatal("missing periodic retry after the claim disappeared")
	}
	f.settings(t, "default", 1)
	if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues("backupTargetName", "waiting")); got != 1 {
		t.Fatalf("backup target waiting count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(f.r.Metrics.Reconciles.WithLabelValues("enabled")); got != 1 {
		t.Fatalf("enabled reconciliations = %v, want 1", got)
	}
}

// An optional annotation present on the triggering read can disappear before
// the field is resolved. That stops management without failing the reconcile.
func TestOptionalAnnotationRemovedBeforePatch(t *testing.T) {
	f := newFixture(t)
	f.metadata(t, map[string]string{EnabledLabel: "true", SnapshotMaxCountAnnotation: "10"})
	reads := 0
	f.r.Reader = interceptor.NewClient(f.c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		pvc, ok := obj.(*corev1.PersistentVolumeClaim)
		if !ok {
			return nil
		}
		reads++
		if reads > 1 {
			delete(pvc.Annotations, SnapshotMaxCountAnnotation)
		}
		return nil
	}})
	result, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pvc)})
	if err != nil {
		t.Fatal(err)
	}
	if reads < 2 {
		t.Fatal("field reconcile did not re-read the claim")
	}
	if result.RequeueAfter != f.r.Config.ResyncInterval {
		t.Fatal("enabled claim was not requeued")
	}
	if got := f.specField(t, "snapshotMaxCount"); got != nil {
		t.Fatalf("removed annotation was still applied: %v", got)
	}
	f.settings(t, "rustfs", 2)
	if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues("snapshotMaxCount", "unmanaged")); got != 1 {
		t.Fatalf("unmanaged count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues("snapshotMaxCount", "error")); got != 0 {
		t.Fatalf("unmanaged field counted as an error: %v", got)
	}
}

func TestNonMapTopologyTermIsNotZonePinned(t *testing.T) {
	f := newFixture(t)
	f.updateVolume(t, func(v *unstructured.Unstructured) {
		if err := unstructured.SetNestedSlice(v.Object, []any{"not-a-term"}, "spec", "topologyRequirement"); err != nil {
			t.Fatal(err)
		}
	})
	f.metadata(t, map[string]string{EnabledLabel: "true", ReplicaZoneSoftAntiAffinityAnnotation: "disabled"})
	f.reconcile(t)
	if got := f.specField(t, "replicaZoneSoftAntiAffinity"); got != "disabled" {
		t.Fatalf("replicaZoneSoftAntiAffinity = %#v, want disabled", got)
	}
	if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues("replicaZoneSoftAntiAffinity", "patched")); got != 1 {
		t.Fatalf("patched count = %v, want 1", got)
	}
}

func TestMalformedVolumeFieldsAreNotPatched(t *testing.T) {
	for _, tc := range []struct {
		name         string
		annotations  map[string]string
		mutate       func(*unstructured.Unstructured)
		field        string
		want         any
		wantTarget   string
		wantReplicas int64
		wantPatches  int
		errFields    []string
	}{
		{
			name: "spec is not a map",
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"] = "not-a-map"
			},
			errFields: []string{"backupTargetName", "numberOfReplicas"},
		},
		{
			name:        "dataLocality has the wrong type",
			annotations: map[string]string{ReplicasAnnotation: "4"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["dataLocality"] = int64(1)
			},
			wantTarget: "rustfs", wantReplicas: 1, wantPatches: 1,
			errFields: []string{"numberOfReplicas"},
		},
		{
			name:        "current dataLocality has the wrong type",
			annotations: map[string]string{DataLocalityAnnotation: "best-effort"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["dataLocality"] = int64(1)
			},
			field: "dataLocality", want: int64(1),
			wantTarget: "rustfs", wantReplicas: 1, wantPatches: 1,
			errFields: []string{"dataLocality", "numberOfReplicas"},
		},
		{
			name:        "data layout has the wrong type",
			annotations: map[string]string{ReplicasAnnotation: "4"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["dataLayout"] = "sharded"
			},
			wantTarget: "rustfs", wantReplicas: 1, wantPatches: 1,
			errFields: []string{"numberOfReplicas"},
		},
		{
			name: "legacy clone replica count is not an integer",
			mutate: func(v *unstructured.Unstructured) {
				v.SetLabels(map[string]string{legacyLinkedCloneLabel: "true"})
				v.Object["spec"].(map[string]any)["numberOfReplicas"] = "1"
			},
			field: "numberOfReplicas", want: "1",
			wantTarget: "rustfs", wantPatches: 1,
			errFields: []string{"numberOfReplicas"},
		},
		{
			name:        "locality layout has the wrong type",
			annotations: map[string]string{DataLocalityAnnotation: "best-effort"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["dataLayout"] = "sharded"
			},
			field: "dataLocality", want: "disabled",
			wantTarget: "rustfs", wantReplicas: 1, wantPatches: 1,
			errFields: []string{"dataLocality", "numberOfReplicas"},
		},
		{
			name:        "attached state has the wrong type",
			annotations: map[string]string{DataLocalityAnnotation: "strict-local", ReplicasAnnotation: "1"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["status"].(map[string]any)["state"] = int64(1)
			},
			field: "dataLocality", want: "disabled",
			wantTarget: "rustfs", wantReplicas: 1, wantPatches: 1,
			errFields: []string{"dataLocality"},
		},
		{
			name:        "strict-local replica count has the wrong type",
			annotations: map[string]string{DataLocalityAnnotation: "strict-local", ReplicasAnnotation: "1"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["dataLocality"] = "strict-local"
				v.Object["spec"].(map[string]any)["numberOfReplicas"] = "1"
				v.Object["status"].(map[string]any)["state"] = "detached"
			},
			field: "dataLocality", want: "strict-local",
			wantTarget: "rustfs", wantReplicas: 1, wantPatches: 2,
			errFields: []string{"dataLocality"},
		},
		{
			name:        "access mode has the wrong type",
			annotations: map[string]string{DataLocalityAnnotation: "strict-local", ReplicasAnnotation: "1"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["status"].(map[string]any)["state"] = "detached"
				v.Object["spec"].(map[string]any)["accessMode"] = int64(1)
			},
			field: "dataLocality", want: "disabled",
			wantTarget: "rustfs", wantReplicas: 1, wantPatches: 1,
			errFields: []string{"dataLocality"},
		},
		{
			name:        "migratable has the wrong type",
			annotations: map[string]string{DataLocalityAnnotation: "strict-local", ReplicasAnnotation: "1"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["status"].(map[string]any)["state"] = "detached"
				v.Object["spec"].(map[string]any)["accessMode"] = "rwx"
				v.Object["spec"].(map[string]any)["migratable"] = "yes"
			},
			field: "dataLocality", want: "disabled",
			wantTarget: "rustfs", wantReplicas: 1, wantPatches: 1,
			errFields: []string{"dataLocality"},
		},
		{
			name:        "topology requirement has the wrong type",
			annotations: map[string]string{ReplicaZoneSoftAntiAffinityAnnotation: "disabled"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["topologyRequirement"] = "zone-a"
			},
			field:      "replicaZoneSoftAntiAffinity",
			wantTarget: "rustfs", wantReplicas: 2, wantPatches: 2,
			errFields: []string{"replicaZoneSoftAntiAffinity"},
		},
		{
			name:        "data engine has the wrong type for unmap",
			annotations: map[string]string{UnmapMarkSnapChainRemovedAnnotation: "enabled"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["dataEngine"] = int64(2)
			},
			field:      "unmapMarkSnapChainRemoved",
			wantTarget: "rustfs", wantReplicas: 2, wantPatches: 2,
			errFields: []string{"unmapMarkSnapChainRemoved"},
		},
		{
			name:        "data engine has the wrong type for bandwidth",
			annotations: map[string]string{ReplicaRebuildingBandwidthLimitAnnotation: "100"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["dataEngine"] = int64(2)
			},
			field:      "replicaRebuildingBandwidthLimit",
			wantTarget: "rustfs", wantReplicas: 2, wantPatches: 2,
			errFields: []string{"replicaRebuildingBandwidthLimit"},
		},
		{
			name:        "volume size has the wrong type",
			annotations: map[string]string{SnapshotMaxSizeAnnotation: "3Gi"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["size"] = int64(1073741824)
			},
			field:      "snapshotMaxSize",
			wantTarget: "rustfs", wantReplicas: 2, wantPatches: 2,
			errFields: []string{"snapshotMaxSize"},
		},
		{
			name:        "volume size is not an integer",
			annotations: map[string]string{SnapshotMaxSizeAnnotation: "3Gi"},
			mutate: func(v *unstructured.Unstructured) {
				v.Object["spec"].(map[string]any)["size"] = "big"
			},
			field:      "snapshotMaxSize",
			wantTarget: "rustfs", wantReplicas: 2, wantPatches: 2,
			errFields: []string{"snapshotMaxSize"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.mutate != nil {
				f.updateVolume(t, tc.mutate)
			}
			labels := map[string]string{EnabledLabel: "true"}
			for key, value := range tc.annotations {
				labels[key] = value
			}
			f.metadata(t, labels)
			_, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pvc)})
			if err == nil {
				t.Fatal("malformed volume was reconciled without an error")
			}
			for _, field := range tc.errFields {
				if !strings.Contains(err.Error(), "reconcile "+field) {
					t.Errorf("error %v does not include field %s", err, field)
				}
				if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues(field, "error")); got != 1 {
					t.Errorf("%s error count = %v, want 1", field, got)
				}
			}
			if got := testutil.ToFloat64(f.r.Metrics.Reconciles.WithLabelValues("error")); got != 1 {
				t.Errorf("error reconciliations = %v, want 1", got)
			}
			if tc.name == "spec is not a map" {
				if spec := f.getVolume(t).Object["spec"]; spec != "not-a-map" {
					t.Fatalf("spec = %#v", spec)
				}
			} else if tc.field == "numberOfReplicas" {
				if got := f.specField(t, "backupTargetName"); got != tc.wantTarget {
					t.Errorf("backup target = %#v, want %s", got, tc.wantTarget)
				}
				if got := f.specField(t, "numberOfReplicas"); got != tc.want {
					t.Errorf("replicas = %#v, want %#v", got, tc.want)
				}
			} else {
				if tc.field != "" && f.specField(t, tc.field) != tc.want {
					t.Errorf("%s = %#v, want %#v", tc.field, f.specField(t, tc.field), tc.want)
				}
				f.settings(t, tc.wantTarget, tc.wantReplicas)
			}
			if f.patches != tc.wantPatches {
				t.Errorf("patches = %d, want %d", f.patches, tc.wantPatches)
			}
		})
	}
}
