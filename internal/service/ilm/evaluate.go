package ilm

import (
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// day is the unit of the lifecycle periods.
const day = 24 * time.Hour

// expiryTime returns when a period of days started at t ends: S3 adds the
// days and rounds the result up to the next midnight UTC (an object
// created on Jan 15 at 10:30 with 3 days expires on Jan 19 at 00:00).
func expiryTime(t time.Time, days int) time.Time {
	return t.UTC().Add(time.Duration(days+1) * day).Truncate(day)
}

// matches reports whether the filter selects an object version. Delete
// markers have no tags and no size: the filters on them never match a
// marker.
func (f Filter) matches(key string, tags map[string]string, size int64, deleteMarker bool) bool {
	if !strings.HasPrefix(key, f.Prefix) {
		return false
	}
	if len(f.Tags) > 0 {
		if deleteMarker {
			return false
		}
		for k, v := range f.Tags {
			if got, ok := tags[k]; !ok || got != v {
				return false
			}
		}
	}
	if f.SizeGreaterThan != nil && (deleteMarker || size <= *f.SizeGreaterThan) {
		return false
	}
	if f.SizeLessThan != nil && (deleteMarker || size >= *f.SizeLessThan) {
		return false
	}
	return true
}

// locked reports whether Object Lock protects a version from deletion at
// now: an active retention or a legal hold.
func locked(v domain.ObjectInfo, now time.Time) bool {
	if v.IsDeleteMarker {
		return false
	}
	return v.LegalHold == domain.LegalHoldOn || v.Retention.Active(now)
}

// due is a candidate action: the earliest due wins.
type due struct {
	at     time.Time
	ruleID string
	ok     bool
}

func (d *due) offer(at time.Time, ruleID string) {
	if !d.ok || at.Before(d.at) {
		d.at, d.ruleID, d.ok = at, ruleID, true
	}
}

// currentExpiry returns when the rules of cfg expire the current version v
// (zero when none does).
func currentExpiry(cfg *Configuration, v domain.ObjectInfo) due {
	var d due
	if cfg == nil || v.IsDeleteMarker {
		return d
	}
	for _, r := range cfg.Rules {
		e := r.Expiration
		if !r.Enabled || e == nil || (e.Days == 0 && e.Date.IsZero()) || !r.Filter.matches(v.Key, v.Tags, v.Size, false) {
			continue
		}
		if e.Days > 0 {
			d.offer(expiryTime(v.ModTime, e.Days), r.ID)
		} else {
			d.offer(e.Date, r.ID)
		}
	}
	return d
}

// evaluate returns the actions due at now (see Service.Evaluate).
func evaluate(now time.Time, in EvaluateInput) []Action {
	cfg := in.Config
	if cfg == nil {
		return nil
	}
	var out []Action
	noncurrent := 0
	for i, v := range in.Versions {
		if i == 0 {
			if a, ok := evaluateCurrent(now, in, v); ok {
				out = append(out, a)
			}
			continue
		}
		if a, ok := evaluateNoncurrent(now, cfg, v, in.Versions[i-1].ModTime, noncurrent); ok {
			a.Bucket = in.Bucket
			out = append(out, a)
		}
		noncurrent++
	}
	for _, u := range in.Uploads {
		var d due
		for _, r := range cfg.Rules {
			if !r.Enabled || r.AbortIncompleteUpload == nil || !strings.HasPrefix(u.Key, r.Filter.Prefix) {
				continue
			}
			d.offer(expiryTime(u.Initiated, r.AbortIncompleteUpload.DaysAfterInitiation), r.ID)
		}
		if d.ok && !now.Before(d.at) {
			out = append(out, Action{Type: ActionAbortUpload, RuleID: d.ruleID, Bucket: in.Bucket, Key: u.Key, UploadID: u.UploadID, Due: d.at})
		}
	}
	return out
}

// evaluateCurrent evaluates the current version (or delete marker) of a
// key.
func evaluateCurrent(now time.Time, in EvaluateInput, v domain.ObjectInfo) (Action, bool) {
	act := Action{Bucket: in.Bucket, Key: v.Key, VersionID: v.VersionID, ModTime: v.ModTime}
	if v.IsDeleteMarker {
		// An expired object delete marker: the only version left of its key.
		if len(in.Versions) != 1 {
			return Action{}, false
		}
		for _, r := range in.Config.Rules {
			if r.Enabled && r.Expiration != nil && r.Expiration.ExpiredObjectDeleteMarker && r.Filter.matches(v.Key, nil, 0, true) {
				act.Type, act.RuleID, act.Due = ActionDeleteVersion, r.ID, v.ModTime
				return act, true
			}
		}
		return Action{}, false
	}
	d := currentExpiry(in.Config, v)
	if !d.ok || now.Before(d.at) {
		return Action{}, false
	}
	act.RuleID, act.Due = d.ruleID, d.at
	switch in.Versioning {
	case domain.VersioningEnabled:
		// The version stays, under a delete marker: allowed even when
		// Object Lock protects it.
		act.Type = ActionDeleteMarker
	case domain.VersioningSuspended:
		// The null delete marker replaces the null version: its data
		// goes, which Object Lock forbids.
		if v.VersionID == "null" && locked(v, now) {
			return Action{}, false
		}
		act.Type = ActionDeleteMarker
	default:
		if locked(v, now) {
			return Action{}, false
		}
		act.Type = ActionDeleteVersion
	}
	return act, true
}

// evaluateNoncurrent evaluates the noncurrent version v, the index-th
// noncurrent version of its key (0 is the newest), noncurrent since
// successor (the creation of the next newer version).
func evaluateNoncurrent(now time.Time, cfg *Configuration, v domain.ObjectInfo, successor time.Time, index int) (Action, bool) {
	var d due
	for _, r := range cfg.Rules {
		n := r.NoncurrentExpiration
		if !r.Enabled || n == nil || index < n.NewerNoncurrentVersions || !r.Filter.matches(v.Key, v.Tags, v.Size, v.IsDeleteMarker) {
			continue
		}
		d.offer(expiryTime(successor, n.NoncurrentDays), r.ID)
	}
	if !d.ok || now.Before(d.at) || locked(v, now) {
		return Action{}, false
	}
	return Action{Type: ActionDeleteVersion, RuleID: d.ruleID, Key: v.Key, VersionID: v.VersionID, ModTime: v.ModTime, Due: d.at}, true
}

// mayApply reports whether a rule of cfg can act on the objects of key.
func (c *Configuration) mayApply(key string) bool {
	for _, r := range c.Rules {
		if r.Enabled && (r.Expiration != nil || r.NoncurrentExpiration != nil) && strings.HasPrefix(key, r.Filter.Prefix) {
			return true
		}
	}
	return false
}

// needsVersions reports whether the evaluation of a current version needs
// the other versions of its key: noncurrent rules, or expired delete
// markers to detect.
func (c *Configuration) needsVersions() bool {
	for _, r := range c.Rules {
		if r.Enabled && (r.NoncurrentExpiration != nil || (r.Expiration != nil && r.Expiration.ExpiredObjectDeleteMarker)) {
			return true
		}
	}
	return false
}

// abortsUploads reports whether a rule of cfg aborts uploads.
func (c *Configuration) abortsUploads() bool {
	for _, r := range c.Rules {
		if r.Enabled && r.AbortIncompleteUpload != nil {
			return true
		}
	}
	return false
}
