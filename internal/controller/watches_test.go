package controller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	"github.com/zekihan/hornkeeper/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestVolumeChangePredicate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		want   bool
	}{
		{"detached", func(v *unstructured.Unstructured) {
			v.Object["status"].(map[string]any)["state"] = "detached"
		}, true},
		{"legacy clone restriction cleared", func(v *unstructured.Unstructured) {
			v.SetLabels(nil)
		}, true},
		{"spec change", func(v *unstructured.Unstructured) {
			v.SetGeneration(2)
		}, true},
		{"unrelated status and label changes", func(v *unstructured.Unstructured) {
			v.Object["status"].(map[string]any)["robustness"] = "degraded"
			v.SetLabels(map[string]string{"longhorn.io/legacy-linked-clone": "true", "unrelated": "changed"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := Resource(VolumeGVK)
			old.SetGeneration(1)
			old.SetLabels(map[string]string{"longhorn.io/legacy-linked-clone": "true"})
			old.Object["status"] = map[string]any{"state": "attached", "robustness": "healthy"}
			updated := old.DeepCopy()
			tc.mutate(updated)
			if got := volumeChanges.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}); got != tc.want {
				t.Errorf("enqueue for volume update = %t, want %t", got, tc.want)
			}
		})
	}
	volume := Resource(VolumeGVK)
	if volumeChanges.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: volume}) {
		t.Fatal("untyped volume update enqueued a reconcile")
	}
	if volumeChanges.Update(event.UpdateEvent{ObjectOld: volume, ObjectNew: &corev1.PersistentVolume{}}) {
		t.Fatal("non-volume update enqueued a reconcile")
	}
}

func TestClaimBackupTargetIndex(t *testing.T) {
	r := &Reconciler{Config: config.Config{BackupTarget: "custom-default"}}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   []string
	}{
		{"nil labels", nil, []string{"custom-default"}},
		{"inherited default", map[string]string{EnabledLabel: "true"}, []string{"custom-default"}},
		{"explicit default", map[string]string{BackupTargetAnnotation: "custom-default"}, []string{"custom-default"}},
		{"explicit override", map[string]string{BackupTargetAnnotation: "override"}, []string{"override"}},
		{"explicit empty", map[string]string{BackupTargetAnnotation: ""}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels, annotations := splitMetadata(tc.labels)
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}}
			if got := r.ClaimBackupTargetIndex(pvc); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("index = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDefaultTargetEventMapping(t *testing.T) {
	f := newFixture(t)
	target := Resource(BackupTargetGVK)
	target.SetNamespace(f.r.Config.LonghornNamespace)
	target.SetName(f.r.Config.BackupTarget)
	want := []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(f.pvc)}}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		mapped bool
	}{
		{"inherited default", map[string]string{EnabledLabel: "true"}, true},
		{"explicit default", map[string]string{EnabledLabel: "true", BackupTargetAnnotation: "rustfs"}, true},
		{"explicit override", map[string]string{EnabledLabel: "true", BackupTargetAnnotation: "default"}, false},
		{"explicit empty", map[string]string{EnabledLabel: "true", BackupTargetAnnotation: ""}, false},
		{"disabled", map[string]string{EnabledLabel: "false"}, false},
		{"unlabelled", nil, false},
		{"removed override", map[string]string{EnabledLabel: "true"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.metadata(t, tc.labels)
			got := f.r.ClaimsForTarget(t.Context(), target)
			if tc.mapped && !reflect.DeepEqual(got, want) || !tc.mapped && len(got) != 0 {
				t.Fatalf("target map = %v, mapped = %v", got, tc.mapped)
			}
		})
	}
	target.SetNamespace("foreign")
	if got := f.r.ClaimsForTarget(t.Context(), target); len(got) != 0 {
		t.Fatalf("mapped foreign namespace target: %v", got)
	}
}

func TestOptInPredicate(t *testing.T) {
	claim := func(value string) client.Object {
		pvc := &corev1.PersistentVolumeClaim{}
		if value != "<absent>" {
			pvc.Labels = map[string]string{EnabledLabel: value}
		}
		return pvc
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{{"true", true}, {"false", false}, {"<absent>", false}, {"enabled", true}, {"", true}} {
		if got := optIn.Create(event.CreateEvent{Object: claim(tc.value)}); got != tc.want {
			t.Errorf("create %q = %t, want %t", tc.value, got, tc.want)
		}
	}
	for _, tc := range []struct {
		old, new string
		want     bool
	}{
		{"<absent>", "true", true},
		{"true", "false", true},
		{"false", "<absent>", false},
		{"<absent>", "enabled", true},
		{"<absent>", "", true},
		{"", "", false},
		{"", "<absent>", false},
		{"false", "enabled", true},
		{"enabled", "enabled", false},
		{"enabled", "yes", true},
		{"enabled", "false", false},
	} {
		if got := optIn.Update(event.UpdateEvent{ObjectOld: claim(tc.old), ObjectNew: claim(tc.new)}); got != tc.want {
			t.Errorf("update %q -> %q = %t, want %t", tc.old, tc.new, got, tc.want)
		}
	}
	if optIn.Delete(event.DeleteEvent{Object: claim("true")}) {
		t.Fatal("claim deletion enqueued a reconcile")
	}
	if !optIn.Generic(event.GenericEvent{Object: claim("true")}) {
		t.Fatal("enabled generic event was ignored")
	}
	if optIn.Generic(event.GenericEvent{Object: claim("false")}) {
		t.Fatal("disabled generic event enqueued a reconcile")
	}
	if !optIn.Generic(event.GenericEvent{Object: claim("enabled")}) {
		t.Fatal("unrecognized opt-in generic event was ignored")
	}
}

func TestClaimAndVolumeIndexesSkipUnboundObjects(t *testing.T) {
	if got := ClaimVolumeIndex(&corev1.PersistentVolumeClaim{}); got != nil {
		t.Fatalf("unbound claim index = %v", got)
	}
	named := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}}
	if got := ClaimVolumeIndex(named); !reflect.DeepEqual(got, []string{"pv"}) {
		t.Fatalf("bound claim index = %v", got)
	}
	for _, pv := range []*corev1.PersistentVolume{
		{},
		{Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "other.driver", VolumeHandle: "vol"}}}},
		{Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: LonghornDriver}}}},
	} {
		if got := VolumeHandleIndex(pv); got != nil {
			t.Fatalf("index for %#v = %v", pv.Spec.CSI, got)
		}
	}
	handled := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: LonghornDriver, VolumeHandle: "vol"}}}}
	if got := VolumeHandleIndex(handled); !reflect.DeepEqual(got, []string{"vol"}) {
		t.Fatalf("handle index = %v", got)
	}
}

func TestEventMappingListErrorsEnqueueNothing(t *testing.T) {
	f := newFixture(t)
	f.r.Client = interceptor.NewClient(f.c, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errors.New("index unavailable")
	}})
	var logs bytes.Buffer
	ctx := ctrl.LoggerInto(t.Context(), logr.FromSlogHandler(slog.NewJSONHandler(&logs, nil)))
	if got := f.r.ClaimsForPV(ctx, f.pv); len(got) != 0 {
		t.Fatalf("PV map = %v", got)
	}
	if got := f.r.ClaimsForVolume(ctx, f.volume); len(got) != 0 {
		t.Fatalf("volume map = %v", got)
	}
	target := Resource(BackupTargetGVK)
	target.SetNamespace(f.r.Config.LonghornNamespace)
	target.SetName("rustfs")
	if got := f.r.ClaimsForTarget(ctx, target); len(got) != 0 {
		t.Fatalf("target map = %v", got)
	}
	for _, msg := range []string{"Cannot map PV event to PVCs", "Cannot map Longhorn event to PVs", "Cannot map backup target event to PVCs"} {
		if !bytes.Contains(logs.Bytes(), []byte(msg)) {
			t.Errorf("log missing %q: %s", msg, logs.String())
		}
	}
}
