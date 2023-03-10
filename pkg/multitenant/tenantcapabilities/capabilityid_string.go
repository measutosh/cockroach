// Code generated by "stringer"; DO NOT EDIT.

package tenantcapabilities

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[CanAdminSplit-1]
	_ = x[CanAdminUnsplit-2]
	_ = x[CanViewNodeInfo-3]
	_ = x[CanViewTSDBMetrics-4]
	_ = x[TenantSpanConfigBounds-5]
	_ = x[MaxCapabilityID-5]
}

const _CapabilityID_name = "can_admin_splitcan_admin_unsplitcan_view_node_infocan_view_tsdb_metricsspan_config_bounds"

var _CapabilityID_index = [...]uint8{0, 15, 32, 50, 71, 89}

func (i CapabilityID) String() string {
	i -= 1
	if i >= CapabilityID(len(_CapabilityID_index)-1) {
		return "CapabilityID(" + strconv.FormatInt(int64(i+1), 10) + ")"
	}
	return _CapabilityID_name[_CapabilityID_index[i]:_CapabilityID_index[i+1]]
}