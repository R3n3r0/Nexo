package accessrule

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/r3n3r0/nexo/internal/service/audit"
)

// ruleDiff is the difference between two lists of rules, by id.
type ruleDiff struct {
	Added   []Rule       `json:"added,omitempty"`
	Removed []Rule       `json:"removed,omitempty"`
	Changed []ruleChange `json:"changed,omitempty"`
}

// ruleChange is a rule whose content changed.
type ruleChange struct {
	ID     string `json:"id"`
	Before Rule   `json:"before"`
	After  Rule   `json:"after"`
}

// empty reports no difference.
func (d ruleDiff) empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// diffRules compares the rules before and after a change.
func diffRules(before, after []Rule) ruleDiff {
	old := make(map[string]Rule, len(before))
	for _, r := range before {
		old[r.ID] = r
	}
	var d ruleDiff
	seen := make(map[string]bool, len(after))
	for _, r := range after {
		seen[r.ID] = true
		prev, ok := old[r.ID]
		switch {
		case !ok:
			d.Added = append(d.Added, r)
		case !reflect.DeepEqual(canonical(prev), canonical(r)):
			d.Changed = append(d.Changed, ruleChange{ID: r.ID, Before: prev, After: r})
		}
	}
	for _, r := range before {
		if !seen[r.ID] {
			d.Removed = append(d.Removed, r)
		}
	}
	return d
}

// canonical is the JSON form of a rule, for comparisons.
func canonical(r Rule) string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

// audit records a change of the access configuration of a bucket (the
// cluster when bucket is empty): who, when (the audit entry) and what
// (the diff), in the log and, when configured, in the audit service.
func (s *service) audit(ctx context.Context, operation, bucket, actor string, diff any) {
	if d, ok := diff.(ruleDiff); ok && d.empty() {
		diff = map[string]any{}
	}
	s.deps.Logger.InfoContext(ctx, "accessrule: access configuration changed",
		"operation", operation, "bucket", bucket, "actor", actor, "diff", diff)
	if s.deps.Audit == nil {
		return
	}
	entry := audit.Entry{
		Version:   audit.EntryVersion,
		Time:      s.deps.Clock.Now().UTC(),
		API:       audit.API{Name: operation, Bucket: bucket, Status: "OK"},
		AccessKey: actor,
		Tags:      map[string]any{"diff": diff},
	}
	if err := s.deps.Audit.Log(ctx, entry); err != nil {
		s.deps.Logger.WarnContext(ctx, "accessrule: audit entry not recorded", "operation", operation, "bucket", bucket, "err", err)
	}
}
