package coord

// Base returns the record an action starts from on view. A failover starts
// from the highest undetermined record when there is one: voters that do not
// answer may have committed it above the effective record or next to a forced
// one of its generation. Any other action, and a failover without such a
// record, starts from the effective record when it tells the active
// datacenter, see [ActiveKnown]. Base returns nil when there is no record to
// start from, also when the undetermined records a failover would start from
// name different datacenters.
func Base(view View, failover bool) *Record {
	if failover {
		if top, ok := UndeterminedTop(view); top != nil {
			if !ok {
				return nil
			}

			return top
		}
	}

	if !ActiveKnown(view) {
		return nil
	}

	return view.Effective
}

// UndeterminedTop returns the highest undetermined record of view, nil when
// there is none, and false when the undetermined records of its generation
// name different datacenters.
func UndeterminedTop(view View) (*Record, bool) {
	u := view.Undetermined
	if len(u) == 0 {
		return nil, true
	}

	for _, r := range u[1:] {
		if r.State.Generation == u[0].State.Generation && r.State.ActiveDatacenter != u[0].State.ActiveDatacenter {
			return &u[0], false
		}
	}

	return &u[0], true
}
