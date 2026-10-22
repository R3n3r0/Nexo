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
// markers have no tags (a tag filter never matches them) and no data: the
// size filters see them as 0 bytes, as S3 does (ObjectSizeLessThan=1 with
// NoncurrentVersionExpiration expires the noncurrent delete markers).
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
	if deleteMarker {
		size = 0
	}
	if f.SizeGreaterThan != nil && size <= *f.SizeGreaterThan {
		return false
	}
	if f.SizeLessThan != nil && size >= *f.SizeLessThan {
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
			} else if a, ok := currentTransition(now, cfg, v); ok {
				a.Bucket = in.Bucket
				out = append(out, a)
			}
			continue
		}
		if a, ok := evaluateNoncurrent(now, cfg, v, in.Versions[i-1].ModTime, noncurrent); ok {
			a.Bucket = in.Bucket
			out = append(out, a)
		} else if a, ok := noncurrentTransition(now, cfg, v, in.Versions[i-1].ModTime, noncurrent); ok {
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
		// An expired object delete marker: the only version left of its
		// key. ExpiredObjectDeleteMarker removes it at once; as in S3, an
		// Expiration in Days removes it too, once it is as old as the
		// rule's period.
		if len(in.Versions) != 1 {
			return Action{}, false
		}
		var d due
		for _, r := range in.Config.Rules {
			e := r.Expiration
			if !r.Enabled || e == nil || !r.Filter.matches(v.Key, nil, 0, true) {
				continue
			}
			switch {
			case e.ExpiredObjectDeleteMarker:
				d.offer(v.ModTime, r.ID)
			case e.Days > 0:
				d.offer(expiryTime(v.ModTime, e.Days), r.ID)
			}
		}
		if !d.ok || now.Before(d.at) {
			return Action{}, false
		}
		act.Type, act.RuleID, act.Due = ActionDeleteVersion, d.ruleID, d.at
		return act, true
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
		if isNullVersion(v.VersionID) && locked(v, now) {
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

// isNullVersion reports the null version: "null", or no id at all (the
// unversioned buckets, and any caller that did not label it).
func isNullVersion(id string) bool { return id == "" || id == domain.NullVersionID }

// mayApply reports whether a rule of cfg can act on the objects of key.
func (c *Configuration) mayApply(key string) bool {
	for _, r := range c.Rules {
		acts := r.Expiration != nil || r.NoncurrentExpiration != nil || len(r.Transitions) > 0 || len(r.NoncurrentTransitions) > 0
		if r.Enabled && acts && strings.HasPrefix(key, r.Filter.Prefix) {
			return true
		}
	}
	return false
}

// needsVersions reports whether the evaluation of a current version needs
// the other versions of its key: noncurrent rules, or expired delete
// markers to detect (ExpiredObjectDeleteMarker, or an Expiration in Days).
func (c *Configuration) needsVersions() bool {
	for _, r := range c.Rules {
		if !r.Enabled {
			continue
		}
		if e := r.Expiration; r.NoncurrentExpiration != nil || len(r.NoncurrentTransitions) > 0 || (e != nil && (e.ExpiredObjectDeleteMarker || e.Days > 0)) {
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

// canTransition reports whether a version may move to a tier: an object
// with data stored on the drives (not inline, not empty, not in a tier
// already).
func canTransition(v domain.ObjectInfo) bool {
	if v.IsDeleteMarker || v.Inlined || v.Size == 0 {
		return false
	}
	return v.StorageClass == "" || v.StorageClass == domain.StorageClassStandard || v.StorageClass == domain.StorageClassReducedRedundancy
}

// pick is a candidate transition: the earliest due wins, then the latest
// declared.
type pick struct {
	due
	class string
}

func (p *pick) offer(at time.Time, ruleID, class string) {
	if !p.ok || at.Before(p.at) {
		p.at, p.ruleID, p.ok, p.class = at, ruleID, true, class
	}
}

// currentTransition returns the transition due on the current version v.
func currentTransition(now time.Time, cfg *Configuration, v domain.ObjectInfo) (Action, bool) {
	if !canTransition(v) {
		return Action{}, false
	}
	var p pick
	for _, r := range cfg.Rules {
		if !r.Enabled || !r.Filter.matches(v.Key, v.Tags, v.Size, false) {
			continue
		}
		for _, t := range r.Transitions {
			if t.Date.IsZero() {
				p.offer(expiryTime(v.ModTime, t.Days), r.ID, t.StorageClass)
			} else {
				p.offer(t.Date, r.ID, t.StorageClass)
			}
		}
	}
	if !p.ok || now.Before(p.at) {
		return Action{}, false
	}
	return Action{Type: ActionTransition, RuleID: p.ruleID, Key: v.Key, VersionID: v.VersionID, ModTime: v.ModTime, Due: p.at, StorageClass: p.class}, true
}

// noncurrentTransition returns the transition due on the noncurrent
// version v (see evaluateNoncurrent).
func noncurrentTransition(now time.Time, cfg *Configuration, v domain.ObjectInfo, successor time.Time, index int) (Action, bool) {
	if !canTransition(v) {
		return Action{}, false
	}
	var p pick
	for _, r := range cfg.Rules {
		if !r.Enabled || !r.Filter.matches(v.Key, v.Tags, v.Size, false) {
			continue
		}
		for _, t := range r.NoncurrentTransitions {
			if index < t.NewerNoncurrentVersions {
				continue
			}
			p.offer(expiryTime(successor, t.NoncurrentDays), r.ID, t.StorageClass)
		}
	}
	if !p.ok || now.Before(p.at) {
		return Action{}, false
	}
	return Action{Type: ActionTransition, RuleID: p.ruleID, Key: v.Key, VersionID: v.VersionID, ModTime: v.ModTime, Due: p.at, StorageClass: p.class}, true
}
