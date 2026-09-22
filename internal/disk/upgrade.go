package disk

// DataDiskUpgradeExceedsParity reports whether a new data disk of
// newSize bytes would leave at least one of the array's current parity
// disks (parityBytes, each disk's own size) too small for it (doc 02
// §4, Q71: "When a new data disk would be larger than the current
// parity, the flow offers [a parity upgrade] first"). It is exactly
// Validate's own ErrParityTooSmall rule ("a parity disk must be at
// least as large as the largest data disk", Q20) evaluated against one
// candidate new disk rather than a whole TopologyPlan, so a caller
// deciding whether to offer the parity-upgrade flow first uses the same
// rule array setup already enforces, not a second one.
func DataDiskUpgradeExceedsParity(newSize int64, parityBytes []int64) bool {
	for _, p := range parityBytes {
		if p < newSize {
			return true
		}
	}
	return false
}
