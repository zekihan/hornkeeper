package controller

import (
	"reflect"
	"testing"

	"github.com/zekihan/hornkeeper/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestClaimBackupTargetIndex(t *testing.T) {
	r := &Reconciler{Config: config.Config{BackupTarget: "custom-default"}}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   []string
	}{
		{"nil labels", nil, []string{"custom-default"}},
		{"inherited default", map[string]string{EnabledLabel: "true"}, []string{"custom-default"}},
		{"explicit default", map[string]string{BackupTargetLabel: "custom-default"}, []string{"custom-default"}},
		{"explicit override", map[string]string{BackupTargetLabel: "override"}, []string{"override"}},
		{"explicit empty", map[string]string{BackupTargetLabel: ""}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Labels: tc.labels}}
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
		{"explicit default", map[string]string{EnabledLabel: "true", BackupTargetLabel: "rustfs"}, true},
		{"explicit override", map[string]string{EnabledLabel: "true", BackupTargetLabel: "default"}, false},
		{"explicit empty", map[string]string{EnabledLabel: "true", BackupTargetLabel: ""}, false},
		{"disabled", map[string]string{EnabledLabel: "false"}, false},
		{"unlabelled", nil, false},
		{"removed override", map[string]string{EnabledLabel: "true"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.labels(t, tc.labels)
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
