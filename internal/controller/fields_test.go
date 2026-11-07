package controller

import (
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (f *fixture) specField(t *testing.T, field string) any {
	t.Helper()
	value, _, err := unstructured.NestedFieldNoCopy(f.getVolume(t).Object, "spec", field)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (f *fixture) updateVolume(t *testing.T, mutate func(v *unstructured.Unstructured)) {
	t.Helper()
	v := f.getVolume(t)
	mutate(v)
	if err := f.c.Update(t.Context(), v); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalFieldsLifecycle(t *testing.T) {
	for _, tc := range []struct {
		label, value, field string
		want                any
	}{
		{DataLocalityLabel, "best-effort", "dataLocality", "best-effort"},
		{ReplicaAutoBalanceLabel, "least-effort", "replicaAutoBalance", "least-effort"},
		{ReplicaSoftAntiAffinityLabel, "enabled", "replicaSoftAntiAffinity", "enabled"},
		{ReplicaZoneSoftAntiAffinityLabel, "disabled", "replicaZoneSoftAntiAffinity", "disabled"},
		{ReplicaDiskSoftAntiAffinityLabel, "ignored", "replicaDiskSoftAntiAffinity", "ignored"},
		{SnapshotDataIntegrityLabel, "fast-check", "snapshotDataIntegrity", "fast-check"},
		{UnmapMarkSnapChainRemovedLabel, "enabled", "unmapMarkSnapChainRemoved", "enabled"},
		{OfflineRebuildingLabel, "enabled", "offlineRebuilding", "enabled"},
		{FreezeFilesystemForSnapshotLabel, "disabled", "freezeFilesystemForSnapshot", "disabled"},
		{SnapshotMaxCountLabel, "250", "snapshotMaxCount", int64(250)},
		{SnapshotMaxSizeLabel, "2Gi", "snapshotMaxSize", "2147483648"},
		{SnapshotMaxSizeLabel, "0", "snapshotMaxSize", "0"},
		{RebuildConcurrentSyncLimitLabel, "5", "rebuildConcurrentSyncLimit", int64(5)},
		{ReplicaRebuildingBandwidthLimitLabel, "0", "replicaRebuildingBandwidthLimit", int64(0)},
		{UblkQueueDepthLabel, "128", "ublkQueueDepth", int64(128)},
		{UblkNumberOfQueueLabel, "4", "ublkNumberOfQueue", int64(4)},
	} {
		t.Run(tc.label+"="+tc.value, func(t *testing.T) {
			f := newFixture(t)
			f.reconcile(t)
			if got := f.specField(t, tc.field); got != nil && tc.field != "dataLocality" {
				t.Fatalf("unlabelled optional field was set: %v", got)
			}
			before := f.patches
			f.labels(t, map[string]string{EnabledLabel: "true", tc.label: tc.value})
			f.reconcile(t)
			if got := f.specField(t, tc.field); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %s=%#v, want %#v", tc.field, got, tc.want)
			}
			if f.patches != before+1 {
				t.Fatalf("expected one patch, got %d", f.patches-before)
			}
			if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues(tc.field, "patched")); got != 1 {
				t.Fatalf("patched metric = %v", got)
			}
			f.reconcile(t)
			if f.patches != before+1 {
				t.Fatal("no-op reconcile wrote a patch")
			}
			// Removing an optional label stops enforcement and keeps the value.
			f.labels(t, map[string]string{EnabledLabel: "true"})
			f.updateVolume(t, func(v *unstructured.Unstructured) {
				unstructured.SetNestedField(v.Object, "manual", "spec", tc.field)
			})
			f.reconcile(t)
			if got := f.specField(t, tc.field); got != "manual" {
				t.Fatalf("unlabelled field was enforced: %v", got)
			}
		})
	}
}

func TestOptionalFieldsDriftAndDisable(t *testing.T) {
	f := newFixture(t)
	f.labels(t, map[string]string{EnabledLabel: "true", SnapshotMaxCountLabel: "100", ReplicaAutoBalanceLabel: "best-effort"})
	f.reconcile(t)
	f.updateVolume(t, func(v *unstructured.Unstructured) {
		unstructured.SetNestedField(v.Object, int64(10), "spec", "snapshotMaxCount")
		unstructured.SetNestedField(v.Object, "disabled", "spec", "replicaAutoBalance")
	})
	f.reconcile(t)
	if f.specField(t, "snapshotMaxCount") != int64(100) || f.specField(t, "replicaAutoBalance") != "best-effort" {
		t.Fatal("drift was not corrected")
	}
	f.labels(t, map[string]string{EnabledLabel: "false", SnapshotMaxCountLabel: "50"})
	f.reconcile(t)
	if f.specField(t, "snapshotMaxCount") != int64(100) {
		t.Fatal("disabled claim was enforced")
	}
}

func TestOptionalFieldsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name, label, value, field string
		mutate                    func(v *unstructured.Unstructured)
	}{
		{"empty enum", ReplicaAutoBalanceLabel, "", "replicaAutoBalance", nil},
		{"unknown enum", ReplicaAutoBalanceLabel, "Best-Effort", "replicaAutoBalance", nil},
		{"enum outside field", ReplicaSoftAntiAffinityLabel, "best-effort", "replicaSoftAntiAffinity", nil},
		{"integrity mode outside other fields", OfflineRebuildingLabel, "fast-check", "offlineRebuilding", nil},
		{"low snapshot count", SnapshotMaxCountLabel, "1", "snapshotMaxCount", nil},
		{"high snapshot count", SnapshotMaxCountLabel, "251", "snapshotMaxCount", nil},
		{"signed snapshot count", SnapshotMaxCountLabel, "+5", "snapshotMaxCount", nil},
		{"empty snapshot count", SnapshotMaxCountLabel, "", "snapshotMaxCount", nil},
		{"high sync limit", RebuildConcurrentSyncLimitLabel, "6", "rebuildConcurrentSyncLimit", nil},
		{"overflow bandwidth", ReplicaRebuildingBandwidthLimitLabel, "99999999999999999999", "replicaRebuildingBandwidthLimit", nil},
		{"small ublk depth", UblkQueueDepthLabel, "16", "ublkQueueDepth", nil},
		{"large ublk queues", UblkNumberOfQueueLabel, "2147483648", "ublkNumberOfQueue", nil},
		{"bad size", SnapshotMaxSizeLabel, "lots", "snapshotMaxSize", nil},
		{"negative size", SnapshotMaxSizeLabel, "-1Gi", "snapshotMaxSize", nil},
		{"fractional size", SnapshotMaxSizeLabel, "1m", "snapshotMaxSize", nil},
		{"size below twice volume", SnapshotMaxSizeLabel, "2047Mi", "snapshotMaxSize", nil},
		{"bandwidth on v1", ReplicaRebuildingBandwidthLimitLabel, "100", "replicaRebuildingBandwidthLimit", nil},
		{"unmap on v2", UnmapMarkSnapChainRemovedLabel, "enabled", "unmapMarkSnapChainRemoved", func(v *unstructured.Unstructured) {
			unstructured.SetNestedField(v.Object, "v2", "spec", "dataEngine")
		}},
		{"zone pinned", ReplicaZoneSoftAntiAffinityLabel, "disabled", "replicaZoneSoftAntiAffinity", func(v *unstructured.Unstructured) {
			unstructured.SetNestedSlice(v.Object, []any{map[string]any{"zone": "a"}, map[string]any{"zone": "a", "region": "r"}}, "spec", "topologyRequirement")
		}},
		{"strict local attached", DataLocalityLabel, "strict-local", "dataLocality", nil},
		{"strict local replicas", DataLocalityLabel, "strict-local", "dataLocality", func(v *unstructured.Unstructured) {
			unstructured.SetNestedField(v.Object, "detached", "status", "state")
			unstructured.SetNestedField(v.Object, int64(3), "spec", "numberOfReplicas")
		}},
		{"leave strict local attached", DataLocalityLabel, "best-effort", "dataLocality", func(v *unstructured.Unstructured) {
			unstructured.SetNestedField(v.Object, "strict-local", "spec", "dataLocality")
		}},
		{"sharded locality", DataLocalityLabel, "best-effort", "dataLocality", func(v *unstructured.Unstructured) {
			unstructured.SetNestedField(v.Object, "sharded", "spec", "dataLayout", "type")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.mutate != nil {
				f.updateVolume(t, tc.mutate)
			}
			before := f.specField(t, tc.field)
			f.labels(t, map[string]string{EnabledLabel: "true", tc.label: tc.value})
			f.reconcile(t)
			if got := f.specField(t, tc.field); !reflect.DeepEqual(got, before) {
				t.Fatalf("invalid value changed %s to %#v", tc.field, got)
			}
			if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues(tc.field, "invalid")); got != 1 {
				t.Fatalf("invalid metric = %v", got)
			}
			// Invalid optional fields do not block the default-managed fields.
			if tc.field != "dataLocality" {
				f.settings(t, "rustfs", 2)
			}
		})
	}
}

func TestOptionalFieldsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name, label, value, field string
		want                      any
		mutate                    func(v *unstructured.Unstructured)
	}{
		{"bandwidth on v2", ReplicaRebuildingBandwidthLimitLabel, "100", "replicaRebuildingBandwidthLimit", int64(100), func(v *unstructured.Unstructured) {
			unstructured.SetNestedField(v.Object, "v2", "spec", "dataEngine")
		}},
		{"unmap disabled on v2", UnmapMarkSnapChainRemovedLabel, "disabled", "unmapMarkSnapChainRemoved", "disabled", func(v *unstructured.Unstructured) {
			unstructured.SetNestedField(v.Object, "v2", "spec", "dataEngine")
		}},
		{"zone pinned enabled", ReplicaZoneSoftAntiAffinityLabel, "enabled", "replicaZoneSoftAntiAffinity", "enabled", func(v *unstructured.Unstructured) {
			unstructured.SetNestedSlice(v.Object, []any{map[string]any{"zone": "a"}}, "spec", "topologyRequirement")
		}},
		{"multiple zones", ReplicaZoneSoftAntiAffinityLabel, "disabled", "replicaZoneSoftAntiAffinity", "disabled", func(v *unstructured.Unstructured) {
			unstructured.SetNestedSlice(v.Object, []any{map[string]any{"zone": "a"}, map[string]any{"zone": "b"}}, "spec", "topologyRequirement")
		}},
		{"region only", ReplicaZoneSoftAntiAffinityLabel, "disabled", "replicaZoneSoftAntiAffinity", "disabled", func(v *unstructured.Unstructured) {
			unstructured.SetNestedSlice(v.Object, []any{map[string]any{"region": "r"}}, "spec", "topologyRequirement")
		}},
		{"strict local detached", DataLocalityLabel, "strict-local", "dataLocality", "strict-local", func(v *unstructured.Unstructured) {
			unstructured.SetNestedField(v.Object, "detached", "status", "state")
		}},
		{"size exactly twice volume", SnapshotMaxSizeLabel, "2Gi", "snapshotMaxSize", "2147483648", nil},
		{"zero ublk depth", UblkQueueDepthLabel, "0", "ublkQueueDepth", int64(0), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.mutate != nil {
				f.updateVolume(t, tc.mutate)
			}
			f.labels(t, map[string]string{EnabledLabel: "true", ReplicasLabel: "1", tc.label: tc.value})
			f.reconcile(t)
			if got := f.specField(t, tc.field); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %s=%#v, want %#v", tc.field, got, tc.want)
			}
		})
	}
}

func TestLeavingStrictLocalRaisesReplicasInOnePass(t *testing.T) {
	f := newFixture(t)
	f.updateVolume(t, func(v *unstructured.Unstructured) {
		unstructured.SetNestedField(v.Object, "strict-local", "spec", "dataLocality")
		unstructured.SetNestedField(v.Object, "detached", "status", "state")
	})
	f.labels(t, map[string]string{EnabledLabel: "true", DataLocalityLabel: "best-effort", ReplicasLabel: "3"})
	f.reconcile(t)
	if got := f.specField(t, "dataLocality"); got != "best-effort" {
		t.Fatalf("dataLocality = %v", got)
	}
	f.settings(t, "rustfs", 3)
}

func TestOptionalFieldsDryRun(t *testing.T) {
	f := newFixture(t)
	f.r.Config.DryRun = true
	f.labels(t, map[string]string{EnabledLabel: "true", SnapshotMaxCountLabel: "10"})
	f.reconcile(t)
	if f.patches != 0 || f.specField(t, "snapshotMaxCount") != nil {
		t.Fatal("dry run changed the volume")
	}
	if got := testutil.ToFloat64(f.r.Metrics.Fields.WithLabelValues("snapshotMaxCount", "dry-run")); got != 1 {
		t.Fatalf("dry-run metric = %v", got)
	}
}

func TestVolumeFieldLabelsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range volumeFields {
		if seen[f.label] || seen[f.name] {
			t.Fatalf("duplicate field or label: %s %s", f.name, f.label)
		}
		seen[f.label], seen[f.name] = true, true
	}
}
