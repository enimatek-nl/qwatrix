package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PushRule is one notification rule.
//
// Rules are per-account and per-kind, and the kind is what orders them: `override` is consulted
// first and can silence everything, `underride` is the last resort. Priority is stored rather than
// derived because a client may reorder rules explicitly (`?before=`/`?after=`), and a re-ordering
// that has to be recomputed from timestamps on every read is a re-ordering that will not survive
// the next restart.
type PushRule struct {
	Localpart string `gorm:"size:255;primaryKey:pushrule_pk" json:"-"`
	Scope     string `gorm:"size:32;primaryKey:pushrule_pk" json:"-"`
	Kind      string `gorm:"size:32;primaryKey:pushrule_pk" json:"kind"`
	RuleID    string `gorm:"size:255;primaryKey:pushrule_pk" json:"rule_id"`
	// Default marks a rule the server ships and the client may only enable or disable. Deleting
	// one is allowed and remembered, which is why deletion flips the flag instead of dropping
	// the row: a server that resurrected a rule the user turned off the next time it started
	// would be quietly overwriting their choice.
	Default bool `gorm:"column:is_default" json:"default"`
	// Removed keeps that memory: a default rule the account removed is recorded as gone rather
	// than absent, so the defaults are not re-added underneath them.
	//
	// The Go name is not `Deleted`, because a field by that name is GORM's soft-delete marker:
	// every query in this file would then carry a condition nobody wrote, and a filter that works
	// by accident is one that breaks the moment the schema moves.
	Removed bool `gorm:"column:deleted" json:"-"`
	Enabled bool `json:"enabled"`
	// Pattern is a content rule's match string; Actions and Conditions are JSON arrays.
	Pattern    string `gorm:"size:255" json:"pattern,omitempty"`
	Actions    string `gorm:"type:text" json:"-"`
	Conditions string `gorm:"type:text" json:"-"`
	// Priority orders the rules within their kind, lowest first.
	Priority int64 `gorm:"index" json:"-"`
	// StreamPos is when the account's rules last changed, which is what lets an incremental
	// /sync decide whether to re-send the whole rule set.
	StreamPos int64 `gorm:"index" json:"-"`
}

// TableName pins the table name.
func (PushRule) TableName() string { return "push_rules" }

var pushRuleConflict = clause.OnConflict{
	Columns:   []clause.Column{{Name: "localpart"}, {Name: "scope"}, {Name: "kind"}, {Name: "rule_id"}},
	UpdateAll: true,
}

// PushRuleRepo is the push-rule repository.
type PushRuleRepo interface {
	// List returns an account's rules in a scope, ordered by kind and priority. Rules the
	// account removed are not returned: a caller reading the rule set wants the rules, not the
	// history of what was taken out of it.
	List(ctx context.Context, localpart, scope string) ([]PushRule, error)
	// ListIncludingRemoved also returns the rules the account removed, which is the only way to
	// tell "this account has no such rule" from "this account had that rule and threw it away".
	// Reconciling a shipped set of defaults against an account needs that distinction, and
	// nothing else should.
	ListIncludingRemoved(ctx context.Context, localpart, scope string) ([]PushRule, error)
	// Get returns one rule, or [ErrNotFound].
	Get(ctx context.Context, localpart, scope, kind, ruleID string) (*PushRule, error)
	// Delete removes a rule row outright.
	Delete(ctx context.Context, localpart, scope, kind, ruleID string) error
	// Put stores or replaces a rule.
	Put(ctx context.Context, rule *PushRule) error
	// Touch stamps the account's rules as changed at a stream position, which is how a /sync
	// long-poller learns that something moved.
	Touch(ctx context.Context, localpart, scope string, position int64) error
	// LastPosition returns when the account's rules last changed, or 0.
	LastPosition(ctx context.Context, localpart, scope string) (int64, error)
}

// PushRuleRepoImpl is the GORM-backed [PushRuleRepo].
type PushRuleRepoImpl struct{ db *gorm.DB }

// NewPushRuleRepo builds the push-rule repository.
func NewPushRuleRepo(db *gorm.DB) *PushRuleRepoImpl { return &PushRuleRepoImpl{db: db} }

// List implements [PushRuleRepo].
func (r *PushRuleRepoImpl) List(ctx context.Context, localpart, scope string) ([]PushRule, error) {
	var rows []PushRule
	err := session(ctx, r.db).
		Where("localpart = ? AND scope = ? AND deleted = ?", localpart, scope, false).
		Order("kind ASC, priority ASC, rule_id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading push rules of %q: %w", localpart, err)
	}
	return rows, nil
}

// Get implements [PushRuleRepo].
// ListIncludingRemoved returns every row, tombstones included, in the same order as [List].
func (r *PushRuleRepoImpl) ListIncludingRemoved(ctx context.Context, localpart, scope string) ([]PushRule, error) {
	var rows []PushRule
	err := session(ctx, r.db).
		Where("localpart = ? AND scope = ?", localpart, scope).
		Order("kind ASC, priority ASC, rule_id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("reading the push rules of %q: %w", localpart, err)
	}
	return rows, nil
}

func (r *PushRuleRepoImpl) Get(ctx context.Context, localpart, scope, kind, ruleID string) (*PushRule, error) {
	var row PushRule
	err := session(ctx, r.db).
		Where("localpart = ? AND scope = ? AND kind = ? AND rule_id = ?", localpart, scope, kind, ruleID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("push rule %q: %w", ruleID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading push rule %q: %w", ruleID, err)
	}
	if row.Removed {
		return nil, fmt.Errorf("push rule %q: %w", ruleID, ErrNotFound)
	}
	return &row, nil
}

// Delete implements [PushRuleRepo].
func (r *PushRuleRepoImpl) Delete(ctx context.Context, localpart, scope, kind, ruleID string) error {
	err := session(ctx, r.db).
		Where("localpart = ? AND scope = ? AND kind = ? AND rule_id = ?", localpart, scope, kind, ruleID).
		Delete(&PushRule{}).Error
	if err != nil {
		return fmt.Errorf("deleting push rule %q: %w", ruleID, err)
	}
	return nil
}

// Put implements [PushRuleRepo].
func (r *PushRuleRepoImpl) Put(ctx context.Context, rule *PushRule) error {
	err := session(ctx, r.db).Clauses(pushRuleConflict).Save(rule).Error
	if err != nil {
		return fmt.Errorf("storing push rule %q: %w", rule.RuleID, err)
	}
	return nil
}

// Touch implements [PushRuleRepo].
func (r *PushRuleRepoImpl) Touch(ctx context.Context, localpart, scope string, position int64) error {
	err := session(ctx, r.db).Model(&PushRule{}).
		Where("localpart = ? AND scope = ?", localpart, scope).
		Update("stream_pos", position).Error
	if err != nil {
		return fmt.Errorf("stamping push rules of %q: %w", localpart, err)
	}
	return nil
}

// LastPosition implements [PushRuleRepo].
func (r *PushRuleRepoImpl) LastPosition(ctx context.Context, localpart, scope string) (int64, error) {
	var position int64
	err := session(ctx, r.db).Model(&PushRule{}).
		Where("localpart = ? AND scope = ?", localpart, scope).
		Select("COALESCE(MAX(stream_pos), 0)").Scan(&position).Error
	if err != nil {
		return 0, fmt.Errorf("reading the last change of push rules of %q: %w", localpart, err)
	}
	return position, nil
}
