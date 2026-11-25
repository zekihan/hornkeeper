package controller

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Optional field annotations. Unlike backup target and replicas, these have no
// controller default: a missing annotation leaves the volume field unmanaged.
const (
	strictLocal = "strict-local"

	// Longhorn bounds annotation values.
	snapshotMaxCountMax                       = 250
	rebuildConcurrentSyncLimitMax             = 5
	ublkQueueDepthMin                         = 32
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
	// Parse is nil for fields with a controller default; those are resolved by
	// dedicated functions that also consult deployment configuration.
	parse func(string) (any, error)
	check func(volume *unstructured.Unstructured, desired any) error
	// Longhorn omits unset ublk fields when serializing a Volume.
	zeroWhenAbsent bool
}

func (f volumeField) optional() bool { return f.parse != nil }

// volumeFields lists every managed Volume spec field in reconciliation order.
// DataLocality precedes numberOfReplicas so leaving strict-local and raising the
// replica count converges in one pass.
var volumeFields = []volumeField{
	{name: "backupTargetName", annotation: BackupTargetAnnotation},
	{
		name:       "dataLocality",
		annotation: DataLocalityAnnotation,
		parse:      enum("disabled", "best-effort", "strict-local"),
		check:      checkDataLocality,
	},
	{name: "numberOfReplicas", annotation: ReplicasAnnotation},
	{
		name:       "replicaAutoBalance",
		annotation: ReplicaAutoBalanceAnnotation,
		parse:      enum("ignored", "disabled", "least-effort", "best-effort"),
	},
	{
		name:       "replicaSoftAntiAffinity",
		annotation: ReplicaSoftAntiAffinityAnnotation,
		parse:      enum("ignored", "enabled", "disabled"),
	},
	{
		name:       "replicaZoneSoftAntiAffinity",
		annotation: ReplicaZoneSoftAntiAffinityAnnotation,
		parse:      enum("ignored", "enabled", "disabled"),
		check:      checkZoneSoftAntiAffinity,
	},
	{
		name:       "replicaDiskSoftAntiAffinity",
		annotation: ReplicaDiskSoftAntiAffinityAnnotation,
		parse:      enum("ignored", "enabled", "disabled"),
	},
	{
		name:       "snapshotDataIntegrity",
		annotation: SnapshotDataIntegrityAnnotation,
		parse:      enum("ignored", "disabled", "enabled", "fast-check"),
	},
	{
		name:       "unmapMarkSnapChainRemoved",
		annotation: UnmapMarkSnapChainRemovedAnnotation,
		parse:      enum("ignored", "disabled", "enabled"),
		check:      checkUnmapMarkSnapChainRemoved,
	},
	{
		name:       "offlineRebuilding",
		annotation: OfflineRebuildingAnnotation,
		parse:      enum("ignored", "disabled", "enabled"),
	},
	{
		name:       "freezeFilesystemForSnapshot",
		annotation: FreezeFilesystemForSnapshotAnnotation,
		parse:      enum("ignored", "enabled", "disabled"),
	},
	{
		name:       "snapshotMaxCount",
		annotation: SnapshotMaxCountAnnotation,
		parse:      integer(2, snapshotMaxCountMax),
	},
	{
		name:       "snapshotMaxSize",
		annotation: SnapshotMaxSizeAnnotation,
		parse:      byteSize,
		check:      checkSnapshotMaxSize,
	},
	{
		name:       "rebuildConcurrentSyncLimit",
		annotation: RebuildConcurrentSyncLimitAnnotation,
		parse:      integer(0, rebuildConcurrentSyncLimitMax),
	},
	{
		name:       "replicaRebuildingBandwidthLimit",
		annotation: ReplicaRebuildingBandwidthLimitAnnotation,
		parse:      integer(0, math.MaxInt64),
		check:      checkRebuildingBandwidthLimit,
	},
	{
		name:           "ublkQueueDepth",
		annotation:     UblkQueueDepthAnnotation,
		parse:          integer(0, math.MaxInt32),
		check:          checkUblkQueueDepth,
		zeroWhenAbsent: true,
	},
	{
		name:           "ublkNumberOfQueue",
		annotation:     UblkNumberOfQueueAnnotation,
		parse:          integer(0, math.MaxInt32),
		zeroWhenAbsent: true,
	},
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
		if value == "" ||
			strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
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
	if bytes, ok := quantity.AsInt64(); ok && bytes >= 0 {
		return strconv.FormatInt(bytes, 10), nil
	}

	// The decimal representation can round fractions or clamp oversized binary
	// quantities. Use the original number when the fast path is unavailable.
	number := value
	var unit big.Rat
	unit.SetInt64(1)
	if quantity.Format != resource.DecimalExponent {
		suffix := strings.TrimLeft(value, "+-.0123456789")
		number = strings.TrimSuffix(value, suffix)
		multiplier := resource.MustParse("1" + suffix)
		if _, ok := unit.SetString(multiplier.AsDec().String()); !ok {
			return nil, errors.New(
				"expected a whole, non-negative byte quantity within int64 range",
			)
		}
	}
	var bytes big.Rat
	if _, ok := bytes.SetString(number); ok {
		bytes.Mul(&bytes, &unit)
		if bytes.IsInt() && bytes.Num().IsInt64() && bytes.Sign() >= 0 {
			return bytes.Num().String(), nil
		}
	}
	return nil, errors.New("expected a whole, non-negative byte quantity within int64 range")
}

// stringValue extracts a desired string value, rejecting wrong-typed values.
func stringValue(desired any) (string, error) {
	value, ok := desired.(string)
	if !ok {
		return "", fmt.Errorf("expected desired string, got %T", desired)
	}

	return value, nil
}

// int64Value extracts a desired int64 value, rejecting wrong-typed values.
func int64Value(desired any) (int64, error) {
	value, ok := desired.(int64)
	if !ok {
		return 0, fmt.Errorf("expected desired int64, got %T", desired)
	}

	return value, nil
}

// volumeString reads a nested string field, discarding the found flag.
func volumeString(volume *unstructured.Unstructured, fields ...string) (string, error) {
	value, _, err := unstructured.NestedString(volume.Object, fields...)

	return value, err
}

// volumeInt64 reads a nested int64 field, discarding the found flag.
func volumeInt64(volume *unstructured.Unstructured, fields ...string) (int64, error) {
	value, _, err := unstructured.NestedInt64(volume.Object, fields...)

	return value, err
}

func checkDataLocality(volume *unstructured.Unstructured, desired any) error {
	locality, err := stringValue(desired)
	if err != nil {
		return invalid(err)
	}

	layout, err := volumeString(volume, "spec", "dataLayout", "type")
	if err != nil {
		return err
	}
	if layout == "sharded" && locality != "disabled" {
		return invalid(
			fmt.Errorf(
				"dataLocality=%s conflicts with sharded data layout, which requires disabled",
				locality,
			),
		)
	}

	current, err := volumeString(volume, "spec", "dataLocality")
	if err != nil {
		return err
	}
	if current != locality && (current == strictLocal || locality == strictLocal) {
		if state, err := volumeString(volume, "status", "state"); err != nil {
			return err
		} else if state != "detached" {
			return invalid(
				fmt.Errorf(
					"dataLocality can only change from %s to %s while the volume is detached",
					current,
					locality,
				),
			)
		}
	}

	if locality != strictLocal {
		return nil
	}

	return checkStrictLocal(volume)
}

// checkStrictLocal validates replica and access-mode constraints for strict-local.
func checkStrictLocal(volume *unstructured.Unstructured) error {
	replicas, err := volumeInt64(volume, "spec", "numberOfReplicas")
	if err != nil {
		return err
	}
	if replicas != 1 {
		return invalid(
			fmt.Errorf("dataLocality=%s requires 1 replica, volume has %d", strictLocal, replicas),
		)
	}

	accessMode, err := volumeString(volume, "spec", "accessMode")
	if err != nil {
		return err
	}
	if accessMode == "rwx" {
		migratable, _, err := unstructured.NestedBool(volume.Object, "spec", "migratable")
		if err != nil {
			return err
		}
		if !migratable {
			return invalid(
				errors.New(
					"dataLocality=strict-local is incompatible with non-migratable RWX volumes",
				),
			)
		}
	}

	return nil
}

func checkZoneSoftAntiAffinity(volume *unstructured.Unstructured, desired any) error {
	value, err := stringValue(desired)
	if err != nil {
		return invalid(err)
	}
	if value == "enabled" {
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
		termZone, ok := fields["zone"].(string)
		if !ok {
			return nil
		}
		if termZone == "" || (zone != "" && termZone != zone) {
			return nil
		}
		zone = termZone
	}
	if zone != "" {
		return invalid(
			fmt.Errorf(
				"replicaZoneSoftAntiAffinity must be enabled for a volume pinned to zone %s",
				zone,
			),
		)
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
	value, err := stringValue(desired)
	if err != nil {
		return invalid(err)
	}
	if engine == "v2" && value != "disabled" {
		return invalid(errors.New("unmapMarkSnapChainRemoved must be disabled for data engine v2"))
	}
	return nil
}

func checkRebuildingBandwidthLimit(volume *unstructured.Unstructured, desired any) error {
	engine, err := dataEngine(volume)
	if err != nil {
		return err
	}
	value, err := int64Value(desired)
	if err != nil {
		return invalid(err)
	}
	if engine != "v2" && value != 0 {
		return invalid(
			errors.New("replicaRebuildingBandwidthLimit is only supported for data engine v2"),
		)
	}
	return nil
}

func checkUblkQueueDepth(_ *unstructured.Unstructured, desired any) error {
	depth, err := int64Value(desired)
	if err != nil {
		return invalid(err)
	}
	if depth != 0 && depth < ublkQueueDepthMin {
		return invalid(fmt.Errorf("ublkQueueDepth must be 0 or at least 32, got %d", depth))
	}
	return nil
}

func checkSnapshotMaxSize(volume *unstructured.Unstructured, desired any) error {
	text, err := stringValue(desired)
	if err != nil {
		return invalid(err)
	}
	limit, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return invalid(err)
	}
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
		return invalid(
			fmt.Errorf(
				"snapshotMaxSize must be 0 or at least twice the volume size (%d bytes), got %d",
				2*bytes,
				limit,
			),
		)
	}
	return nil
}
