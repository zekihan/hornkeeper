package controller

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Optional field annotations. Unlike backup target and replicas, these have no
// controller default: a missing annotation leaves the volume field unmanaged.
const (
	DataLocalityAnnotation                    = keyPrefix + "data-locality"
	ReplicaAutoBalanceAnnotation              = keyPrefix + "replica-auto-balance"
	ReplicaSoftAntiAffinityAnnotation         = keyPrefix + "replica-soft-anti-affinity"
	ReplicaZoneSoftAntiAffinityAnnotation     = keyPrefix + "replica-zone-soft-anti-affinity"
	ReplicaDiskSoftAntiAffinityAnnotation     = keyPrefix + "replica-disk-soft-anti-affinity"
	SnapshotDataIntegrityAnnotation           = keyPrefix + "snapshot-data-integrity"
	UnmapMarkSnapChainRemovedAnnotation       = keyPrefix + "unmap-mark-snap-chain-removed"
	OfflineRebuildingAnnotation               = keyPrefix + "offline-rebuilding"
	FreezeFilesystemForSnapshotAnnotation     = keyPrefix + "freeze-filesystem-for-snapshot"
	SnapshotMaxCountAnnotation                = keyPrefix + "snapshot-max-count"
	SnapshotMaxSizeAnnotation                 = keyPrefix + "snapshot-max-size"
	RebuildConcurrentSyncLimitAnnotation      = keyPrefix + "rebuild-concurrent-sync-limit"
	ReplicaRebuildingBandwidthLimitAnnotation = keyPrefix + "replica-rebuilding-bandwidth-limit"
	UblkQueueDepthAnnotation                  = keyPrefix + "ublk-queue-depth"
	UblkNumberOfQueueAnnotation               = keyPrefix + "ublk-number-of-queue"
)

// errUnmanaged reports that an optional field's annotation disappeared between the
// initial claim read and a retry.
var errUnmanaged = errors.New("field is not managed")

type volumeField struct {
	name       string
	annotation string
	// parse is nil for fields with a controller default; those are resolved by
	// dedicated functions that also consult deployment configuration.
	parse func(string) (any, error)
	check func(volume *unstructured.Unstructured, desired any) error
}

func (f volumeField) optional() bool { return f.parse != nil }

// volumeFields lists every managed Volume spec field in reconciliation order.
// dataLocality precedes numberOfReplicas so leaving strict-local and raising the
// replica count converges in one pass.
var volumeFields = []volumeField{
	{name: "backupTargetName", annotation: BackupTargetAnnotation},
	{name: "dataLocality", annotation: DataLocalityAnnotation, parse: enum("disabled", "best-effort", "strict-local"), check: checkDataLocality},
	{name: "numberOfReplicas", annotation: ReplicasAnnotation},
	{name: "replicaAutoBalance", annotation: ReplicaAutoBalanceAnnotation, parse: enum("ignored", "disabled", "least-effort", "best-effort")},
	{name: "replicaSoftAntiAffinity", annotation: ReplicaSoftAntiAffinityAnnotation, parse: enum("ignored", "enabled", "disabled")},
	{name: "replicaZoneSoftAntiAffinity", annotation: ReplicaZoneSoftAntiAffinityAnnotation, parse: enum("ignored", "enabled", "disabled"), check: checkZoneSoftAntiAffinity},
	{name: "replicaDiskSoftAntiAffinity", annotation: ReplicaDiskSoftAntiAffinityAnnotation, parse: enum("ignored", "enabled", "disabled")},
	{name: "snapshotDataIntegrity", annotation: SnapshotDataIntegrityAnnotation, parse: enum("ignored", "disabled", "enabled", "fast-check")},
	{name: "unmapMarkSnapChainRemoved", annotation: UnmapMarkSnapChainRemovedAnnotation, parse: enum("ignored", "disabled", "enabled"), check: checkUnmapMarkSnapChainRemoved},
	{name: "offlineRebuilding", annotation: OfflineRebuildingAnnotation, parse: enum("ignored", "disabled", "enabled")},
	{name: "freezeFilesystemForSnapshot", annotation: FreezeFilesystemForSnapshotAnnotation, parse: enum("ignored", "enabled", "disabled")},
	{name: "snapshotMaxCount", annotation: SnapshotMaxCountAnnotation, parse: integer(2, 250)},
	{name: "snapshotMaxSize", annotation: SnapshotMaxSizeAnnotation, parse: byteSize, check: checkSnapshotMaxSize},
	{name: "rebuildConcurrentSyncLimit", annotation: RebuildConcurrentSyncLimitAnnotation, parse: integer(0, 5)},
	{name: "replicaRebuildingBandwidthLimit", annotation: ReplicaRebuildingBandwidthLimitAnnotation, parse: integer(0, math.MaxInt64), check: checkRebuildingBandwidthLimit},
	{name: "ublkQueueDepth", annotation: UblkQueueDepthAnnotation, parse: integer(0, math.MaxInt32), check: checkUblkQueueDepth},
	{name: "ublkNumberOfQueue", annotation: UblkNumberOfQueueAnnotation, parse: integer(0, math.MaxInt32)},
}

func enum(values ...string) func(string) (any, error) {
	return func(value string) (any, error) {
		if !slices.Contains(values, value) {
			return nil, fmt.Errorf("expected one of %s", strings.Join(values, ", "))
		}
		return value, nil
	}
}

func integer(lowest, highest int64) func(string) (any, error) {
	return func(value string) (any, error) {
		expected := fmt.Errorf("expected an integer from %d to %d", lowest, highest)
		// Only decimal digits are accepted; signs and whitespace are rejected.
		if value == "" || strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
			return nil, expected
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < lowest || parsed > highest {
			return nil, expected
		}
		return parsed, nil
	}
}

// byteSize accepts a non-negative Kubernetes quantity such as 0, 10Gi or 500M and
// returns the decimal byte string Longhorn stores for int64 string fields.
func byteSize(value string) (any, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("expected a byte quantity such as 0 or 20Gi: %w", err)
	}
	bytes, ok := quantity.AsInt64()
	if !ok || bytes < 0 {
		return nil, fmt.Errorf("expected a whole, non-negative byte quantity")
	}
	return strconv.FormatInt(bytes, 10), nil
}

func checkDataLocality(volume *unstructured.Unstructured, desired any) error {
	locality := desired.(string)
	layout, _, err := unstructured.NestedString(volume.Object, "spec", "dataLayout", "type")
	if err != nil {
		return err
	}
	if layout == "sharded" && locality != "disabled" {
		return invalid(fmt.Errorf("dataLocality=%s conflicts with sharded data layout, which requires disabled", locality))
	}
	current, _, err := unstructured.NestedString(volume.Object, "spec", "dataLocality")
	if err != nil {
		return err
	}
	if current != locality && (current == "strict-local" || locality == "strict-local") {
		state, _, err := unstructured.NestedString(volume.Object, "status", "state")
		if err != nil {
			return err
		}
		if state != "detached" {
			return invalid(fmt.Errorf("dataLocality can only change from %s to %s while the volume is detached", current, locality))
		}
	}
	if locality == "strict-local" {
		replicas, _, err := unstructured.NestedInt64(volume.Object, "spec", "numberOfReplicas")
		if err != nil {
			return err
		}
		if replicas != 1 {
			return invalid(fmt.Errorf("dataLocality=strict-local requires 1 replica, volume has %d", replicas))
		}
	}
	return nil
}

func checkZoneSoftAntiAffinity(volume *unstructured.Unstructured, desired any) error {
	if desired.(string) == "enabled" {
		return nil
	}
	terms, _, err := unstructured.NestedSlice(volume.Object, "spec", "topologyRequirement")
	if err != nil {
		return err
	}
	// Mirrors Longhorn's IsTopologyZonePinned: every term names the same zone.
	zone := ""
	for _, term := range terms {
		fields, ok := term.(map[string]any)
		if !ok {
			return nil
		}
		termZone, _ := fields["zone"].(string)
		if termZone == "" || (zone != "" && termZone != zone) {
			return nil
		}
		zone = termZone
	}
	if zone != "" {
		return invalid(fmt.Errorf("replicaZoneSoftAntiAffinity must be enabled for a volume pinned to zone %s", zone))
	}
	return nil
}

func dataEngine(volume *unstructured.Unstructured) (string, error) {
	engine, _, err := unstructured.NestedString(volume.Object, "spec", "dataEngine")
	return engine, err
}

func checkUnmapMarkSnapChainRemoved(volume *unstructured.Unstructured, desired any) error {
	engine, err := dataEngine(volume)
	if err != nil {
		return err
	}
	if engine == "v2" && desired.(string) != "disabled" {
		return invalid(fmt.Errorf("unmapMarkSnapChainRemoved must be disabled for data engine v2"))
	}
	return nil
}

func checkRebuildingBandwidthLimit(volume *unstructured.Unstructured, desired any) error {
	engine, err := dataEngine(volume)
	if err != nil {
		return err
	}
	if engine != "v2" && desired.(int64) != 0 {
		return invalid(fmt.Errorf("replicaRebuildingBandwidthLimit is only supported for data engine v2"))
	}
	return nil
}

func checkUblkQueueDepth(_ *unstructured.Unstructured, desired any) error {
	if depth := desired.(int64); depth != 0 && depth < 32 {
		return invalid(fmt.Errorf("ublkQueueDepth must be 0 or at least 32, got %d", depth))
	}
	return nil
}

func checkSnapshotMaxSize(volume *unstructured.Unstructured, desired any) error {
	limit, _ := strconv.ParseInt(desired.(string), 10, 64)
	if limit == 0 {
		return nil
	}
	size, _, err := unstructured.NestedString(volume.Object, "spec", "size")
	if err != nil {
		return err
	}
	bytes, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		return fmt.Errorf("parse volume size %q: %w", size, err)
	}
	if limit/2 < bytes {
		return invalid(fmt.Errorf("snapshotMaxSize must be 0 or at least twice the volume size (%d bytes), got %d", 2*bytes, limit))
	}
	return nil
}
