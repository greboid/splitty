// Package group holds the Group type, its store and the group pages
// (detail, members, settings, settle-up). A group of kind "direct" is a
// hidden per-pair ledger used for non-group (friend) expenses.
package group

import (
	"time"
)

// Kind enumerates the two group flavours.
const (
	KindGroup  = "group"
	KindDirect = "direct"
)

type Group struct {
	ID        int64
	Name      string
	Kind      string
	CreatedBy int64
	CreatedAt time.Time
}

// IsDirect reports whether this is a hidden friend ledger.
func (g Group) IsDirect() bool { return g.Kind == KindDirect }
