package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zekihan/hornkeeper/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type fixture struct {
	r       *Reconciler
	c       client.WithWatch
	pvc     *corev1.PersistentVolumeClaim
	pv      *corev1.PersistentVolume
	volume  *unstructured.Unstructured
	patches int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	f := &fixture{}
	f.pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "app", UID: "claim-uid", Labels: map[string]string{EnabledLabel: "true"}},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	f.pv = &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef:               &corev1.ObjectReference{Namespace: "app", Name: "claim", UID: "claim-uid"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: LonghornDriver, VolumeHandle: "restored-volume"}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	f.volume = Resource(VolumeGVK)
	f.volume.SetName("restored-volume")
	f.volume.SetNamespace("longhorn")
	f.volume.SetUID("volume-uid")
	f.volume.SetLabels(map[string]string{"keep": "this"})
	f.volume.Object["spec"] = map[string]any{
		"backupTargetName": "default", "numberOfReplicas": int64(1), "dataLocality": "disabled",
		"size": "1073741824", "nodeID": "node-a", "fromBackup": "s3://old-backup", "encrypted": true,
	}
	f.volume.Object["status"] = map[string]any{"state": "attached", "robustness": "healthy"}
	defaultTarget, rustfsTarget := Resource(BackupTargetGVK), Resource(BackupTargetGVK)
	defaultTarget.SetName("default")
	defaultTarget.SetNamespace("longhorn")
	rustfsTarget.SetName("rustfs")
	rustfsTarget.SetNamespace("longhorn")
	f.c = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(f.pvc, f.pv, f.volume, defaultTarget, rustfsTarget).
		WithStatusSubresource(f.pvc, f.pv).
		WithIndex(&corev1.PersistentVolumeClaim{}, claimVolumeIndex, ClaimVolumeIndex).
		WithIndex(&corev1.PersistentVolume{}, volumeHandleIndex, VolumeHandleIndex).
		WithIndex(&corev1.PersistentVolumeClaim{}, claimBackupTargetIndex, ClaimBackupTargetIndex).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			f.patches++
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			var body map[string]any
			if err := json.Unmarshal(data, &body); err != nil {
				return err
			}
			spec, ok := body["spec"].(map[string]any)
			if !ok || len(spec) != 1 {
				t.Fatalf("patch must own exactly one field: %s", data)
			}
			for key := range spec {
				if key != "backupTargetName" && key != "numberOfReplicas" {
					t.Fatalf("unowned field %q", key)
				}
			}
			metadata, ok := body["metadata"].(map[string]any)
			if !ok || metadata["resourceVersion"] == nil || len(metadata) != 1 {
				t.Fatalf("missing optimistic lock or unrelated metadata: %s", data)
			}
			if len(body) != 2 {
				t.Fatalf("unexpected patch content: %s", data)
			}
			return c.Patch(ctx, obj, patch, opts...)
		}}).Build()
	cfg := config.Default()
	cfg.BackupTarget = "rustfs"
	cfg.Replicas = 2
	f.r = &Reconciler{Client: f.c, Reader: f.c, Config: cfg, Metrics: NewMetrics(prometheus.NewRegistry())}
	return f
}

func (f *fixture) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	result, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pvc)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func (f *fixture) labels(t *testing.T, labels map[string]string) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := f.c.Get(t.Context(), client.ObjectKeyFromObject(f.pvc), pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Labels = labels
	if err := f.c.Update(t.Context(), pvc); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) getVolume(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	v := Resource(VolumeGVK)
	if err := f.c.Get(t.Context(), client.ObjectKeyFromObject(f.volume), v); err != nil {
		t.Fatal(err)
	}
	return v
}
func (f *fixture) settings(t *testing.T, target string, replicas int64) {
	t.Helper()
	v := f.getVolume(t)
	actualTarget, _, err := unstructured.NestedString(v.Object, "spec", "backupTargetName")
	if err != nil {
		t.Fatal(err)
	}
	actualReplicas, _, err := unstructured.NestedInt64(v.Object, "spec", "numberOfReplicas")
	if err != nil {
		t.Fatal(err)
	}
	if actualTarget != target || actualReplicas != replicas {
		t.Fatalf("got target=%q replicas=%d, want %q %d", actualTarget, actualReplicas, target, replicas)
	}
}

func TestOptIn(t *testing.T) {
	for _, value := range []string{"", "false", "TRUE", "True", "1", "yes", " true"} {
		t.Run(value, func(t *testing.T) {
			f := newFixture(t)
			labels := map[string]string{BackupTargetLabel: "rustfs", ReplicasLabel: "2"}
			if value != "" {
				labels[EnabledLabel] = value
			}
			f.labels(t, labels)
			f.r.Reader = interceptor.NewClient(f.c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); !ok {
					t.Fatal("disabled claim must not read PV or Longhorn objects")
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			if result := f.reconcile(t); result.RequeueAfter != 0 {
				t.Fatal("disabled PVC was requeued")
			}
			f.settings(t, "default", 1)
			if f.patches != 0 {
				t.Fatal("disabled PVC was patched")
			}
		})
	}
}

func TestDefaultsOverridesRemovalDisablingAndDrift(t *testing.T) {
	f := newFixture(t)
	before := f.getVolume(t).DeepCopy()
	if result := f.reconcile(t); result.RequeueAfter != f.r.Config.ResyncInterval {
		t.Fatal("missing periodic retry")
	}
	f.settings(t, "rustfs", 2)
	patched := f.patches
	f.reconcile(t)
	if f.patches != patched {
		t.Fatal("no-op reconcile wrote a patch")
	}
	f.labels(t, map[string]string{EnabledLabel: "true", BackupTargetLabel: "default", ReplicasLabel: "3"})
	f.reconcile(t)
	f.settings(t, "default", 3)
	f.labels(t, map[string]string{EnabledLabel: "true", ReplicasLabel: "3"})
	f.reconcile(t)
	f.settings(t, "rustfs", 3)
	f.labels(t, map[string]string{EnabledLabel: "true"})
	f.reconcile(t)
	f.settings(t, "rustfs", 2)
	v := f.getVolume(t)
	v.Object["spec"].(map[string]any)["numberOfReplicas"] = int64(4)
	v.Object["spec"].(map[string]any)["backupTargetName"] = "default"
	if err := f.c.Update(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	// A fresh Reconciler has no previous-process state but still corrects drift.
	copy := *f.r
	f.r = &copy
	f.reconcile(t)
	f.settings(t, "rustfs", 2)
	for _, labels := range []map[string]string{{EnabledLabel: "false", ReplicasLabel: "5"}, {ReplicasLabel: "5"}} {
		f.labels(t, labels)
		f.reconcile(t)
		f.settings(t, "rustfs", 2)
	}
	after := f.getVolume(t)
	for _, obj := range []*unstructured.Unstructured{before, after} {
		unstructured.RemoveNestedField(obj.Object, "spec", "backupTargetName")
		unstructured.RemoveNestedField(obj.Object, "spec", "numberOfReplicas")
		obj.SetResourceVersion("")
	}
	if !reflect.DeepEqual(before.Object, after.Object) {
		t.Fatalf("unrelated fields changed: before=%v after=%v", before.Object, after.Object)
	}
}

func TestInvalidFieldsAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name             string
		labels           map[string]string
		locality, layout string
		target           string
		replicas         int64
	}{
		{"empty replica", map[string]string{ReplicasLabel: ""}, "", "", "rustfs", 1},
		{"zero replica", map[string]string{ReplicasLabel: "0"}, "", "", "rustfs", 1},
		{"negative replica", map[string]string{ReplicasLabel: "-1"}, "", "", "rustfs", 1},
		{"large replica", map[string]string{ReplicasLabel: "21"}, "", "", "rustfs", 1},
		{"overflow replica", map[string]string{ReplicasLabel: "999999999999999999999999"}, "", "", "rustfs", 1},
		{"decimal replica", map[string]string{ReplicasLabel: "2.0"}, "", "", "rustfs", 1},
		{"signed replica", map[string]string{ReplicasLabel: "+2"}, "", "", "rustfs", 1},
		{"whitespace replica", map[string]string{ReplicasLabel: " 2"}, "", "", "rustfs", 1},
		{"empty target", map[string]string{BackupTargetLabel: ""}, "", "", "default", 2},
		{"bad target", map[string]string{BackupTargetLabel: "BAD_target"}, "", "", "default", 2},
		{"missing target", map[string]string{BackupTargetLabel: "missing"}, "", "", "default", 2},
		{"both invalid", map[string]string{BackupTargetLabel: "missing", ReplicasLabel: "nope"}, "", "", "default", 1},
		{"strict local default", nil, "strict-local", "", "rustfs", 1},
		{"strict local override", map[string]string{ReplicasLabel: "3"}, "strict-local", "", "rustfs", 1},
		{"strict local valid", map[string]string{ReplicasLabel: "1"}, "strict-local", "", "rustfs", 1},
		{"sharded", nil, "disabled", "sharded", "rustfs", 1},
		{"max replicas", map[string]string{ReplicasLabel: "20"}, "", "", "rustfs", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			labels := map[string]string{EnabledLabel: "true"}
			for k, v := range tc.labels {
				labels[k] = v
			}
			f.labels(t, labels)
			v := f.getVolume(t)
			if tc.locality != "" {
				unstructured.SetNestedField(v.Object, tc.locality, "spec", "dataLocality")
			}
			if tc.layout != "" {
				unstructured.SetNestedField(v.Object, tc.layout, "spec", "dataLayout", "type")
			}
			if err := f.c.Update(t.Context(), v); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			f.settings(t, tc.target, tc.replicas)
		})
	}
}

func TestBindingAndVolumeResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*fixture)
		allowed bool
	}{
		{"restored handle", func(*fixture) {}, true},
		{"pending", func(f *fixture) { f.pvc.Status.Phase = corev1.ClaimPending }, false},
		{"no volume name", func(f *fixture) { f.pvc.Spec.VolumeName = "" }, false},
		{"foreign CSI", func(f *fixture) { f.pv.Spec.CSI.Driver = "other.driver" }, false},
		{"non CSI", func(f *fixture) { f.pv.Spec.CSI = nil }, false},
		{"empty handle", func(f *fixture) { f.pv.Spec.CSI.VolumeHandle = "" }, false},
		{"missing volume", func(f *fixture) { f.pv.Spec.CSI.VolumeHandle = "missing" }, false},
		{"missing PV", func(f *fixture) { f.pvc.Spec.VolumeName = "missing" }, false},
		{"released PV", func(f *fixture) { f.pv.Status.Phase = corev1.VolumeReleased }, false},
		{"different claim UID", func(f *fixture) { f.pv.Spec.ClaimRef.UID = "previous-claim" }, false},
		{"different claim namespace", func(f *fixture) { f.pv.Spec.ClaimRef.Namespace = "other" }, false},
		{"different claim name", func(f *fixture) { f.pv.Spec.ClaimRef.Name = "other" }, false},
		{"missing claim ref", func(f *fixture) { f.pv.Spec.ClaimRef = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.mutate(f)
			pvcStatus, pvStatus := f.pvc.Status, f.pv.Status
			if err := f.c.Update(t.Context(), f.pvc); err != nil {
				t.Fatal(err)
			}
			f.pvc.Status = pvcStatus
			if err := f.c.Status().Update(t.Context(), f.pvc); err != nil {
				t.Fatal(err)
			}
			if err := f.c.Update(t.Context(), f.pv); err != nil {
				t.Fatal(err)
			}
			f.pv.Status = pvStatus
			if err := f.c.Status().Update(t.Context(), f.pv); err != nil {
				t.Fatal(err)
			}
			// UID-derived names are decoys, even for restored PVCs.
			decoy := f.volume.DeepCopy()
			decoy.SetName("pvc-claim-uid")
			decoy.SetResourceVersion("")
			if err := f.c.Create(t.Context(), decoy); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if tc.allowed {
				f.settings(t, "rustfs", 2)
			} else {
				f.settings(t, "default", 1)
			}
			if err := f.c.Get(t.Context(), client.ObjectKeyFromObject(decoy), decoy); err != nil {
				t.Fatal(err)
			}
			if n, _, _ := unstructured.NestedInt64(decoy.Object, "spec", "numberOfReplicas"); n != 1 {
				t.Fatal("modified UID-derived decoy")
			}
		})
	}
}

func TestPendingClaimLaterBinds(t *testing.T) {
	f := newFixture(t)
	pvc := &corev1.PersistentVolumeClaim{}
	f.c.Get(t.Context(), client.ObjectKeyFromObject(f.pvc), pvc)
	pvc.Status.Phase = corev1.ClaimPending
	f.c.Status().Update(t.Context(), pvc)
	f.reconcile(t)
	f.settings(t, "default", 1)
	pvc.Status.Phase = corev1.ClaimBound
	f.c.Status().Update(t.Context(), pvc)
	f.reconcile(t)
	f.settings(t, "rustfs", 2)
}

func TestConflictRechecksOptInAndPreservesConcurrentFields(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(fmt.Sprintf("disable=%v", disable), func(t *testing.T) {
			f := newFixture(t)
			attempts := 0
			f.r.Client = interceptor.NewClient(f.c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				attempts++
				if attempts == 1 {
					current := f.getVolume(t)
					unstructured.SetNestedField(current.Object, "concurrent-node", "spec", "nodeID")
					if err := c.Update(ctx, current); err != nil {
						return err
					}
					if disable {
						f.labels(t, map[string]string{EnabledLabel: "false"})
					}
					return apierrors.NewConflict(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, obj.GetName(), fmt.Errorf("injected conflict"))
				}
				return c.Patch(ctx, obj, patch, opts...)
			}})
			f.reconcile(t)
			if disable {
				f.settings(t, "default", 1)
				if attempts != 1 {
					t.Fatalf("patched after disabling: %d attempts", attempts)
				}
			} else {
				f.settings(t, "rustfs", 2)
				if attempts != 3 {
					t.Fatalf("expected conflict and two successful fields, got %d", attempts)
				}
			}
			if node, _, _ := unstructured.NestedString(f.getVolume(t).Object, "spec", "nodeID"); node != "concurrent-node" {
				t.Fatal("concurrent unrelated field lost")
			}
		})
	}
}

func TestPatchRejectionDoesNotBlockOtherField(t *testing.T) {
	f := newFixture(t)
	f.r.Client = interceptor.NewClient(f.c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		body, err := patch.Data(obj)
		if err != nil {
			return err
		}
		var data map[string]any
		json.Unmarshal(body, &data)
		if _, ok := data["spec"].(map[string]any)["backupTargetName"]; ok {
			return apierrors.NewForbidden(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, obj.GetName(), fmt.Errorf("admission rejected field"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	_, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pvc)})
	if err == nil {
		t.Fatal("patch rejection must trigger retry")
	}
	f.settings(t, "default", 2)
}

func TestRetryExhaustion(t *testing.T) {
	f := newFixture(t)
	attempts := 0
	f.r.Client = interceptor.NewClient(f.c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		attempts++
		return apierrors.NewConflict(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, obj.GetName(), fmt.Errorf("conflict"))
	}})
	_, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pvc)})
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected retryable conflict, got %v", err)
	}
	if attempts < 4 {
		t.Fatalf("expected retries, got %d", attempts)
	}
	f.settings(t, "default", 1)
}

func TestBackupTargetAndLegacyValidation(t *testing.T) {
	f := newFixture(t)
	if err := CheckBackupTarget(t.Context(), f.c, "longhorn", "missing"); !apierrors.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
	if err := CheckBackupTarget(t.Context(), f.c, "longhorn", "default"); err != nil {
		t.Fatal(err)
	}
	v := f.getVolume(t)
	v.SetLabels(map[string]string{"longhorn.io/legacy-linked-clone": "true"})
	f.c.Update(t.Context(), v)
	f.reconcile(t)
	f.settings(t, "rustfs", 1)
	f.labels(t, map[string]string{EnabledLabel: "true", BackupTargetLabel: "missing"})
	f.reconcile(t)
	f.settings(t, "rustfs", 1)
	// Target creation makes the previously invalid override actionable.
	target := Resource(BackupTargetGVK)
	target.SetName("missing")
	target.SetNamespace("longhorn")
	f.c.Create(t.Context(), target)
	f.reconcile(t)
	f.settings(t, "missing", 1)
}

func TestEventMapping(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	want := []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: "app", Name: "claim"}}}
	if got := f.r.ClaimsForPV(ctx, f.pv); !reflect.DeepEqual(got, want) {
		t.Fatalf("PV map: %v", got)
	}
	if got := f.r.ClaimsForVolume(ctx, f.volume); !reflect.DeepEqual(got, want) {
		t.Fatalf("volume map: %v", got)
	}
	otherNamespace := f.volume.DeepCopy()
	otherNamespace.SetNamespace("other")
	if got := f.r.ClaimsForVolume(ctx, otherNamespace); len(got) != 0 {
		t.Fatal("mapped foreign namespace")
	}
	// Create a PVC with the backup target label to test ClaimsForTarget field index.
	pvcWithLabel := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-with-label", Namespace: "app", UID: "claim-uid-2", Labels: map[string]string{EnabledLabel: "true", BackupTargetLabel: "default"}},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if err := f.c.Create(ctx, pvcWithLabel); err != nil {
		t.Fatal(err)
	}
	target := Resource(BackupTargetGVK)
	target.SetNamespace("longhorn")
	target.SetName("default")
	wantWithLabel := []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: "app", Name: "claim-with-label"}}}
	if got := f.r.ClaimsForTarget(ctx, target); !reflect.DeepEqual(got, wantWithLabel) {
		t.Fatalf("target map: %v", got)
	}
	// Disable the PVC and verify it's no longer mapped.
	pvcWithLabel.Labels = nil
	if err := f.c.Update(ctx, pvcWithLabel); err != nil {
		t.Fatal(err)
	}
	if got := f.r.ClaimsForTarget(ctx, target); len(got) != 0 {
		t.Fatal("mapped disabled target")
	}
	f.labels(t, nil)
	if got := f.r.ClaimsForPV(ctx, f.pv); len(got) != 0 {
		t.Fatal("mapped disabled PVC")
	}
	if got := f.r.ClaimsForVolume(ctx, f.volume); len(got) != 0 {
		t.Fatal("mapped disabled volume")
	}
}

func TestSingleOverrides(t *testing.T) {
	for _, tc := range []struct {
		label, value, target string
		replicas             int64
	}{
		{BackupTargetLabel, "default", "default", 2},
		{ReplicasLabel, "4", "rustfs", 4},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f := newFixture(t)
			f.labels(t, map[string]string{EnabledLabel: "true", tc.label: tc.value})
			f.reconcile(t)
			f.settings(t, tc.target, tc.replicas)
		})
	}
}

func TestDeletingResourcesAndMissingClaim(t *testing.T) {
	for _, resource := range []string{"claim", "pv", "volume", "target"} {
		t.Run(resource, func(t *testing.T) {
			f := newFixture(t)
			var obj client.Object
			switch resource {
			case "claim":
				obj = &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "claim"}}
			case "pv":
				obj = &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv"}}
			case "volume":
				obj = f.volume.DeepCopy()
			case "target":
				obj = Resource(BackupTargetGVK)
				obj.SetName("rustfs")
				obj.SetNamespace("longhorn")
			}
			if err := f.c.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal(err)
			}
			obj.SetFinalizers([]string{"test/hold"})
			if err := f.c.Update(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
			if err := f.c.Delete(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if resource == "target" {
				f.settings(t, "default", 2)
				if err := CheckBackupTarget(t.Context(), f.c, "longhorn", "rustfs"); err == nil {
					t.Fatal("deleting target accepted at startup")
				}
			} else {
				f.settings(t, "default", 1)
			}
		})
	}
	f := newFixture(t)
	if err := f.c.Delete(t.Context(), f.pvc); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	f.settings(t, "default", 1)
}

func TestConflictRechecksChangedOverrides(t *testing.T) {
	f := newFixture(t)
	first := true
	f.r.Client = interceptor.NewClient(f.c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if first {
			first = false
			f.labels(t, map[string]string{EnabledLabel: "true", BackupTargetLabel: "default", ReplicasLabel: "4"})
			return apierrors.NewConflict(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, obj.GetName(), fmt.Errorf("conflict"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	f.reconcile(t)
	f.settings(t, "default", 4)
}
