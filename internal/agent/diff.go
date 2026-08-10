package agent

import "reflect"

// materializationExempt is the literal exemption list from
// docs/05-schedule-spec.md#edit-scope: "If only title, notes, priority, or
// attrs changed, skip materialization entirely." Deliberately conservative -
// grace_period_minutes and reconcile_at have no obvious effect on occurrence
// placement either, but the spec names exactly these four, and the
// materializer is idempotent, so treating anything outside this list as
// schedule-affecting costs at most a harmless re-plan rather than a missed
// one.
var materializationExempt = map[string]bool{
	"title":    true,
	"notes":    true,
	"priority": true,
	"attrs":    true,
}

// changedFields returns the JSON field names of every non-zero field in c,
// reflecting over c's own json tags so the set stays in lockstep with the
// struct without a second maintained list.
func changedFields(c ItemChanges) []string {
	rv := reflect.ValueOf(c)
	rt := rv.Type()
	var out []string
	for i := 0; i < rt.NumField(); i++ {
		fv := rv.Field(i)
		zero := false
		switch fv.Kind() {
		case reflect.Ptr, reflect.Slice:
			zero = fv.IsNil()
		default:
			zero = fv.IsZero()
		}
		if !zero {
			out = append(out, jsonFieldName(rt.Field(i)))
		}
	}
	return out
}

// needsRematerialize reports whether any changed field falls outside the
// exemption list - the field-level diff that keeps a typo fix from churning
// the calendar.
func needsRematerialize(c ItemChanges) bool {
	for _, f := range changedFields(c) {
		if !materializationExempt[f] {
			return true
		}
	}
	return false
}

// hasNonScheduleChange reports whether any field other than schedule
// changed - scope=single's own diff, since schedule is handled separately
// there (via UpdateOccurrenceOverride) regardless of the exemption list, and
// every other changed field still needs the item row written even though a
// single-occurrence edit never triggers a re-plan.
func hasNonScheduleChange(c ItemChanges) bool {
	for _, f := range changedFields(c) {
		if f != "schedule" {
			return true
		}
	}
	return false
}
